package discountexpr

import (
	"container/list"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/vm"
	"github.com/shopspring/decimal"
)

const MaxExpressionBytes = 4096

// Context 仅保存折扣条件所需的数值和显式参数，不保存请求正文或鉴权头。
type Context struct {
	At                   int64          `json:"at"`
	InputTokens          float64        `json:"input_tokens"`
	OutputTokens         float64        `json:"output_tokens"`
	TaskTokens           float64        `json:"task_tokens"`
	CacheReadTokens      float64        `json:"cache_read_tokens"`
	Seconds              float64        `json:"seconds"`
	Count                float64        `json:"count"`
	Units                float64        `json:"units"`
	Params               map[string]any `json:"params,omitempty"`
	PermanentDiscountBPS int            `json:"-"`
}

type compiled struct {
	source             string
	program            *vm.Program
	params             []string
	timeDependent      bool
	usageDependent     bool
	permanentDependent bool
	locations          map[string]*time.Location
}

var cache = struct {
	sync.Mutex
	entries map[string]*list.Element
	order   *list.List
}{entries: make(map[string]*list.Element), order: list.New()}

type inspector struct {
	err                           error
	params                        []string
	timeDependent, usageDependent bool
	permanentDependent            bool
	locations                     map[string]*time.Location
}

func (v *inspector) Visit(node *ast.Node) {
	if v.err != nil {
		return
	}
	switch n := (*node).(type) {
	case *ast.IdentifierNode:
		switch n.Value {
		case "input_tokens", "output_tokens", "cache_read_tokens", "task_tokens", "seconds", "count", "units":
			v.usageDependent = true
		case "permanent_discount":
			v.permanentDependent = true
		}
	case *ast.CallNode:
		identifier, ok := n.Callee.(*ast.IdentifierNode)
		if !ok {
			v.err = fmt.Errorf("discount functions must be named")
			return
		}
		switch identifier.Value {
		case "param":
			if len(n.Arguments) != 1 {
				v.err = fmt.Errorf("param requires one literal field")
				return
			}
			key, ok := n.Arguments[0].(*ast.StringNode)
			if !ok || !allowedParam(key.Value) {
				v.err = fmt.Errorf("unsupported discount request parameter")
				return
			}
			v.params = append(v.params, key.Value)
		case "hour", "weekday", "minute":
			v.timeDependent = true
			if len(n.Arguments) != 1 {
				v.err = fmt.Errorf("time function requires a literal timezone")
				return
			}
			zone, ok := n.Arguments[0].(*ast.StringNode)
			if !ok {
				v.err = fmt.Errorf("timezone must be a literal")
				return
			}
			if location, err := time.LoadLocation(zone.Value); err != nil {
				v.err = fmt.Errorf("invalid discount timezone: %w", err)
			} else {
				v.locations[zone.Value] = location
			}
		case "min", "max", "abs", "ceil", "floor":
		default:
			v.err = fmt.Errorf("unsupported discount function %s", identifier.Value)
		}
	case *ast.IntegerNode, *ast.FloatNode, *ast.BoolNode, *ast.StringNode, *ast.NilNode, *ast.UnaryNode, *ast.ConditionalNode:
	case *ast.BinaryNode:
		if n.Operator == ".." || n.Operator == "matches" {
			v.err = fmt.Errorf("unsupported discount operator")
		}
	default:
		v.err = fmt.Errorf("discount expressions support scalar arithmetic and conditions only")
	}
}

func allowedParam(key string) bool {
	// 只冻结与计价有关的公开标量，禁止将消息、凭证或任意用户内容写入任务快照。
	switch key {
	case "model", "service_tier", "quality", "resolution", "size", "duration", "seconds", "n", "generate_audio", "stream", "max_tokens", "max_completion_tokens":
		return true
	}
	return false
}

func environment(c Context, locations map[string]*time.Location) map[string]any {
	zoneTime := func(zone string) time.Time {
		location := locations[zone]
		if location == nil {
			location = time.UTC
		}
		return time.Unix(c.At, 0).In(location)
	}
	return map[string]any{
		"permanent_discount": float64(c.PermanentDiscountBPS) / 10000,
		"input_tokens":       c.InputTokens, "output_tokens": c.OutputTokens, "cache_read_tokens": c.CacheReadTokens,
		"task_tokens": c.TaskTokens,
		"seconds":     c.Seconds, "count": c.Count, "units": c.Units,
		"param":   func(key string) any { return c.Params[key] },
		"hour":    func(zone string) int { return zoneTime(zone).Hour() },
		"minute":  func(zone string) int { return zoneTime(zone).Minute() },
		"weekday": func(zone string) int { return int(zoneTime(zone).Weekday()) },
		"min":     math.Min, "max": math.Max, "abs": math.Abs, "ceil": math.Ceil, "floor": math.Floor,
	}
}

func compile(source string) (*compiled, error) {
	source = strings.TrimSpace(source)
	if source == "" || len(source) > MaxExpressionBytes {
		return nil, fmt.Errorf("discount expression must contain 1–4096 bytes")
	}
	cache.Lock()
	if element := cache.entries[source]; element != nil {
		cache.order.MoveToFront(element)
		entry := element.Value.(*compiled)
		cache.Unlock()
		return entry, nil
	}
	cache.Unlock()
	inspection := &inspector{locations: make(map[string]*time.Location)}
	program, err := expr.Compile(source, expr.Env(environment(Context{}, nil)), expr.AsFloat64(), expr.DisableAllBuiltins(), expr.MaxNodes(256), expr.Patch(inspection))
	if err != nil {
		return nil, fmt.Errorf("invalid discount expression: %w", err)
	}
	if inspection.err != nil {
		return nil, inspection.err
	}
	entry := &compiled{source: source, program: program, params: inspection.params, timeDependent: inspection.timeDependent, usageDependent: inspection.usageDependent, permanentDependent: inspection.permanentDependent, locations: inspection.locations}
	cache.Lock()
	defer cache.Unlock()
	if element := cache.entries[source]; element != nil {
		cache.order.MoveToFront(element)
		return element.Value.(*compiled), nil
	}
	cache.entries[source] = cache.order.PushFront(entry)
	if cache.order.Len() > 512 {
		oldest := cache.order.Back()
		delete(cache.entries, oldest.Value.(*compiled).source)
		cache.order.Remove(oldest)
	}
	return entry, nil
}

func Validate(source string) error {
	entry, err := compile(source)
	if err != nil {
		return err
	}
	if !entry.usageDependent && !entry.timeDependent && !entry.permanentDependent && len(entry.params) == 0 {
		_, err = Evaluate(source, Context{})
	}
	return err
}

func Parameters(source string) ([]string, error) {
	entry, err := compile(source)
	if err != nil {
		return nil, err
	}
	return append([]string(nil), entry.params...), nil
}

func DependsOnUsage(source string) bool {
	entry, err := compile(source)
	return err == nil && entry.usageDependent
}

func DependsOnPermanent(source string) bool {
	entry, err := compile(source)
	return err == nil && entry.permanentDependent
}

func Evaluate(source string, context Context) (int, error) {
	for _, value := range []float64{context.InputTokens, context.OutputTokens, context.CacheReadTokens, context.TaskTokens, context.Seconds, context.Count, context.Units} {
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) || value > math.MaxInt32 {
			return 0, fmt.Errorf("invalid discount usage")
		}
	}
	entry, err := compile(source)
	if err != nil {
		return 0, err
	}
	if entry.permanentDependent && (context.PermanentDiscountBPS < 1 || context.PermanentDiscountBPS > 10000) {
		return 0, fmt.Errorf("permanent discount snapshot is missing or invalid")
	}
	result, err := expr.Run(entry.program, environment(context, entry.locations))
	if err != nil {
		return 0, fmt.Errorf("discount expression evaluation failed: %w", err)
	}
	ratio, ok := result.(float64)
	if !ok || math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio < 0.0001 || ratio > 1 {
		return 0, fmt.Errorf("discount expression must return a ratio between 0.0001 and 1")
	}
	// 输入已限制在 0.0001–1，按十进制四舍五入转换到现有整数基点精度。
	return int(decimal.NewFromFloat(ratio).Shift(4).Round(0).IntPart()), nil
}
