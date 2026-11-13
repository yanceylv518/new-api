package billingexpr

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/expr-lang/expr/ast"
	"github.com/tidwall/gjson"
)

const maxTaskRequestSnapshotBytes = 16 << 10
const taskRequestExpressionPrefix = "task_request_snapshot_v1:"
const taskRequestEmbeddedPrefix = "task_request_snapshot_v2:"

func MarkTaskRequestExpression(expression string) string {
	return taskRequestExpressionPrefix + PublicExpression(expression)
}

// 旧节点会保留已知表达式字段，但会忽略新增的快照字段。
func MarkTaskRequestExpressionWithContext(expression, encrypted string) string {
	return taskRequestEmbeddedPrefix + encrypted + "|" + PublicExpression(expression)
}

func PublicExpression(expression string) string {
	if rest, ok := strings.CutPrefix(expression, taskRequestEmbeddedPrefix); ok {
		if _, body, found := strings.Cut(rest, "|"); found {
			return body
		}
	}
	return strings.TrimPrefix(expression, taskRequestExpressionPrefix)
}

func HasTaskRequestExpression(expression string) bool {
	return strings.HasPrefix(expression, taskRequestExpressionPrefix) || strings.HasPrefix(expression, taskRequestEmbeddedPrefix)
}

func TaskRequestExpressionHash(expression string) string {
	if strings.HasPrefix(expression, taskRequestEmbeddedPrefix) {
		return ExprHashString(taskRequestEmbeddedPrefix + PublicExpression(expression))
	}
	return ExprHashString(expression)
}

func TaskRequestCiphertext(expression, legacy string) (string, error) {
	if rest, ok := strings.CutPrefix(expression, taskRequestEmbeddedPrefix); ok {
		sealed, body, found := strings.Cut(rest, "|")
		if !found || sealed == "" || body == "" || legacy != "" {
			return "", fmt.Errorf("invalid task billing request snapshot")
		}
		return sealed, nil
	}
	if strings.HasPrefix(expression, taskRequestExpressionPrefix) {
		if legacy == "" {
			return "", fmt.Errorf("task billing request snapshot is missing")
		}
		return legacy, nil
	}
	if legacy != "" {
		return "", fmt.Errorf("task billing request snapshot version is missing")
	}
	return "", nil
}

type TaskRequestPlan struct {
	parameters []string
	headers    []string
	time       bool
}

func (p TaskRequestPlan) NeedsInput() bool {
	return len(p.parameters) > 0 || len(p.headers) > 0
}

func (p TaskRequestPlan) NeedsParameters() bool {
	return len(p.parameters) > 0
}

func inspectTaskRequestPlan(node ast.Node) (TaskRequestPlan, error) {
	parameters := map[string]struct{}{}
	headers := map[string]struct{}{}
	plan := TaskRequestPlan{}
	var inspectionErr error
	directCalls := make(map[*ast.IdentifierNode]struct{})
	ast.Find(node, func(node ast.Node) bool {
		call, ok := node.(*ast.CallNode)
		if !ok {
			return false
		}
		callee, ok := call.Callee.(*ast.IdentifierNode)
		if !ok {
			return false
		}
		directCalls[callee] = struct{}{}
		switch callee.Value {
		case "hour", "minute", "weekday", "month", "day":
			plan.time = true
		case "param", "header":
			if len(call.Arguments) != 1 {
				inspectionErr = fmt.Errorf("task pricing %s requires one literal argument", callee.Value)
				return false
			}
			argument, ok := call.Arguments[0].(*ast.StringNode)
			if !ok {
				inspectionErr = fmt.Errorf("task pricing %s requires a literal argument", callee.Value)
				return false
			}
			name := strings.TrimSpace(argument.Value)
			if name == "" {
				return false
			}
			if callee.Value == "param" {
				parameters[name] = struct{}{}
			} else {
				headers[strings.ToLower(name)] = struct{}{}
			}
		}
		return false
	})
	ast.Find(node, func(node ast.Node) bool {
		identifier, ok := node.(*ast.IdentifierNode)
		if !ok {
			return false
		}
		switch identifier.Value {
		case "$env":
			inspectionErr = fmt.Errorf("task pricing cannot access the expression environment directly")
		case "param", "header", "hour", "minute", "weekday", "month", "day":
			if _, direct := directCalls[identifier]; !direct {
				inspectionErr = fmt.Errorf("task pricing %s must be called directly", identifier.Value)
			}
		}
		return false
	})
	for name := range parameters {
		plan.parameters = append(plan.parameters, name)
	}
	for name := range headers {
		plan.headers = append(plan.headers, name)
	}
	slices.Sort(plan.parameters)
	slices.Sort(plan.headers)
	return plan, inspectionErr
}

func PlanTaskRequest(exprStr string) (TaskRequestPlan, error) {
	entry, err := compileEntryFromCacheByHash(exprStr, ExprHashString(exprStr))
	if err != nil {
		return TaskRequestPlan{}, err
	}
	if entry.taskRequestErr != nil {
		return TaskRequestPlan{}, entry.taskRequestErr
	}
	return entry.taskRequest, nil
}

type taskRequestSnapshot struct {
	Parameters map[string]any    `json:"parameters,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	At         int64             `json:"at"`
}

// 只冻结表达式引用的请求值；不复制完整正文或未参与计价的媒体。
func CaptureTaskRequest(plan TaskRequestPlan, input RequestInput, at time.Time) (RequestInput, string, error) {
	if !plan.NeedsInput() && !plan.time {
		return RequestInput{}, "", nil
	}
	if at.IsZero() {
		at = time.Now()
	}
	snapshot := taskRequestSnapshot{
		Parameters: make(map[string]any, len(plan.parameters)),
		Headers:    make(map[string]string, len(plan.headers)),
		At:         at.Unix(),
	}
	for _, path := range plan.parameters {
		if input.Params != nil {
			if value, ok := input.Params[path]; ok {
				snapshot.Parameters[path] = normalizeTaskVideoScalar(path, value, input.NormalizeVideoScalars)
			}
			continue
		}
		value := gjson.GetBytes(input.Body, path)
		if !value.Exists() {
			continue
		}
		if len(value.Raw) > maxTaskRequestSnapshotBytes {
			return RequestInput{}, "", fmt.Errorf("task pricing request parameter is too large")
		}
		snapshot.Parameters[path] = normalizeTaskVideoScalar(path, value.Value(), input.NormalizeVideoScalars)
	}
	normalizedHeaders := normalizeHeaders(input.Headers)
	for _, name := range plan.headers {
		if value, ok := normalizedHeaders[name]; ok {
			snapshot.Headers[name] = value
		}
	}
	encoded, err := common.Marshal(snapshot)
	if err != nil {
		return RequestInput{}, "", err
	}
	if len(encoded) > maxTaskRequestSnapshotBytes {
		return RequestInput{}, "", fmt.Errorf("task pricing request snapshot is too large")
	}
	sealed, err := common.SealTaskSecret(string(encoded))
	if err != nil {
		return RequestInput{}, "", err
	}
	return RequestInput{Params: snapshot.Parameters, Headers: snapshot.Headers, At: snapshot.At}, sealed, nil
}

func normalizeTaskVideoScalar(path string, value any, enabled bool) any {
	if !enabled {
		return value
	}
	switch path {
	case "duration", "seconds", "frames", "seed", "priority", "execution_expires_after":
	default:
		return value
	}
	text, ok := value.(string)
	if !ok {
		return value
	}
	text = strings.TrimSpace(text)
	number, err := strconv.ParseFloat(text, 64)
	if err != nil {
		integer, integerErr := strconv.ParseInt(text, 0, 64)
		if integerErr != nil {
			return value
		}
		number = float64(integer)
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number {
		return value
	}
	return number
}

func RestoreTaskRequest(sealed string) (RequestInput, error) {
	if sealed == "" {
		return RequestInput{}, nil
	}
	encoded, err := common.OpenTaskSecret(sealed)
	if err != nil {
		return RequestInput{}, err
	}
	if len(encoded) > maxTaskRequestSnapshotBytes {
		return RequestInput{}, fmt.Errorf("task pricing request snapshot is too large")
	}
	var snapshot taskRequestSnapshot
	if err := common.Unmarshal([]byte(encoded), &snapshot); err != nil {
		return RequestInput{}, fmt.Errorf("invalid task pricing request snapshot: %w", err)
	}
	if snapshot.At == 0 {
		return RequestInput{}, fmt.Errorf("task pricing request snapshot time is missing")
	}
	return RequestInput{Params: snapshot.Parameters, Headers: snapshot.Headers, At: snapshot.At}, nil
}
