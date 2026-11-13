package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
)

const taskCallbackPurpose = "task-callback@1"

type TaskCallbackRequest struct {
	URL       string `json:"url"`
	BodyField string `json:"bodyField"`
}

type TaskCallbackTarget struct {
	Purpose   string `json:"purpose"`
	UserID    int    `json:"user_id"`
	TaskID    string `json:"task_id"`
	PluginKey string `json:"plugin_key"`
	URL       string `json:"url"`
	ExpiresAt int64  `json:"expires_at"`
}

var taskCallbackBodyField = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)
var taskCallbackSlots = make(chan struct{}, 16)

func ValidateTaskCallbackURL(raw string) error {
	if raw == "" || len(raw) > 4096 || strings.ContainsAny(raw, "\\\r\n\t ") {
		return errors.New("callback_url must be a public HTTPS domain URL")
	}
	parsed, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" || parsed.Fragment != "" || strings.Contains(raw, "#") || net.ParseIP(parsed.Hostname()) != nil {
		return errors.New("callback_url must be a public HTTPS domain URL")
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.ParseUint(port, 10, 16)
		if err != nil || number == 0 {
			return errors.New("callback_url port is invalid")
		}
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || !strings.Contains(host, ".") {
		return errors.New("callback_url must be a public HTTPS domain URL")
	}
	for _, character := range host {
		if character > 127 || !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' || character == '.') {
			return errors.New("callback_url must use an ASCII domain name")
		}
	}
	return nil
}

// BuildTaskCallbackURL 将客户地址封装为绑定用户、任务和插件的限时能力地址。
func BuildTaskCallbackURL(userID int, taskID, pluginKey string, request TaskCallbackRequest) (string, error) {
	if userID <= 0 || taskID == "" || len(taskID) > 191 || pluginKey == "" || !taskCallbackBodyField.MatchString(request.BodyField) {
		return "", errors.New("invalid task callback binding")
	}
	if err := ValidateTaskCallbackURL(request.URL); err != nil {
		return "", err
	}
	base := strings.TrimSpace(system_setting.TaskPublicAddress)
	if base == "" {
		base = strings.TrimSpace(system_setting.ServerAddress)
	}
	if err := ValidateTaskCallbackURL(base); err != nil {
		return "", errors.New("configure a public HTTPS task address before using callbacks")
	}
	if err := ValidateTaskArtifactBaseURL(base); err != nil {
		return "", errors.New("task public address is invalid")
	}
	claims := TaskCallbackTarget{
		Purpose: taskCallbackPurpose, UserID: userID, TaskID: taskID, PluginKey: pluginKey,
		URL: request.URL, ExpiresAt: time.Now().Add(4 * 24 * time.Hour).Unix(),
	}
	encoded, err := common.Marshal(claims)
	if err != nil {
		return "", err
	}
	access, err := common.SealTaskSecret(string(encoded))
	if err != nil {
		return "", errors.New("configure a persistent task encryption secret before using callbacks")
	}
	address, _ := url.Parse(base)
	escapedBasePath := strings.TrimRight(address.EscapedPath(), "/")
	address.Scheme = "https"
	address.Path = strings.TrimRight(address.Path, "/") + "/v1/tasks/" + taskID + "/callback"
	address.RawPath = escapedBasePath + "/v1/tasks/" + url.PathEscape(taskID) + "/callback"
	address.RawQuery = url.Values{"access": {access}}.Encode()
	return address.String(), nil
}

func ParseTaskCallbackTarget(access, taskID string) (*TaskCallbackTarget, error) {
	if access == "" || len(access) > 16384 {
		return nil, errors.New("invalid callback access")
	}
	plain, err := common.OpenTaskSecret(access)
	if err != nil || plain == access {
		return nil, errors.New("invalid callback access")
	}
	var target TaskCallbackTarget
	if err := common.Unmarshal([]byte(plain), &target); err != nil || target.Purpose != taskCallbackPurpose || target.UserID <= 0 || target.TaskID != taskID || target.PluginKey == "" || target.ExpiresAt <= time.Now().Unix() {
		return nil, errors.New("invalid callback access")
	}
	if err := ValidateTaskCallbackURL(target.URL); err != nil {
		return nil, errors.New("invalid callback access")
	}
	return &target, nil
}

var taskCallbackHTTPClient = newTaskCallbackHTTPClient()

func newTaskCallbackHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	protectedDialer := &protectedFetchDialer{
		resolver: net.DefaultResolver, dialContext: dialer.DialContext,
		getProtection: func() (*common.SSRFProtection, bool, error) {
			protection, enabled, err := currentFetchProtection()
			if err != nil {
				return nil, true, err
			}
			if !enabled {
				protection = &common.SSRFProtection{}
			}
			copy := *protection
			copy.AllowPrivateIp, copy.ApplyIPFilterForDomain = false, true
			return &copy, true, nil
		},
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: protectedDialer.DialContext, TLSHandshakeTimeout: 5 * time.Second,
			MaxIdleConns: 32, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second,
			MaxResponseHeaderBytes: 16 << 10,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// ForwardTaskCallback 只转换通知中的公开标识，不用回调内容修改任务状态或资金。
func ForwardTaskCallback(ctx context.Context, target *TaskCallbackTarget, task *model.Task, body []byte) error {
	select {
	case taskCallbackSlots <- struct{}{}:
		defer func() { <-taskCallbackSlots }()
	default:
		return errors.New("task callback delivery is busy")
	}
	copy := *task
	copy.Data = body
	view, err := BuildTaskPluginView(&copy)
	if err != nil {
		return errors.New("task callback response is invalid")
	}
	encoded, err := common.Marshal(view.Data)
	if err != nil {
		return errors.New("task callback response is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, bytes.NewReader(encoded))
	if err != nil {
		return errors.New("task callback request is invalid")
	}
	request.URL.Scheme = "https"
	eventID := sha256.Sum256(append([]byte(task.TaskID+"\x00"), body...))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Seedance-Event-ID", fmt.Sprintf("%x", eventID))
	request.Header.Set("X-Seedance-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	response, err := taskCallbackHTTPClient.Do(request)
	if err != nil {
		return errors.New("task callback delivery failed")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("task callback destination did not acknowledge delivery")
	}
	return nil
}
