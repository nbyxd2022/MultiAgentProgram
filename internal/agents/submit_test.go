package agents

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"multiagent/internal/schema"
	"multiagent/internal/tool"
)

func testSpec(t *testing.T) SubmitSpec {
	t.Helper()
	return SubmitSpec{
		ToolName:    "submit_thing",
		Description: "提交测试结论。",
		Parameters: []tool.Parameter{
			{Name: "value", Type: tool.TypeString, Required: true, Description: "结论"},
		},
		Decode: func(args string) (schema.Payload, error) {
			var in struct {
				Value string `json:"value"`
			}
			if err := json.Unmarshal([]byte(args), &in); err != nil {
				return nil, err
			}
			if strings.TrimSpace(in.Value) == "" {
				return nil, errEmptyValue
			}
			return schema.Research{Topic: in.Value}, nil
		},
	}
}

var errEmptyValue = &testError{"value 不能为空"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

func TestNewSubmitToolValidates(t *testing.T) {
	good := testSpec(t)

	cases := []struct {
		name string
		mut  func(*SubmitSpec)
	}{
		{"缺少工具名", func(s *SubmitSpec) { s.ToolName = "  " }},
		{"没有声明任何参数", func(s *SubmitSpec) { s.Parameters = nil }},
		{"没有解码函数", func(s *SubmitSpec) { s.Decode = nil }},
	}
	for _, c := range cases {
		spec := good
		c.mut(&spec)
		if _, err := NewSubmitTool(spec); err == nil {
			t.Errorf("%s：应当在构造期就被拒绝", c.name)
		}
	}
	if _, err := NewSubmitTool(good); err != nil {
		t.Fatalf("合法 Spec 不应报错: %v", err)
	}
}

func TestSubmitToolRecordsProduct(t *testing.T) {
	st, _ := NewSubmitTool(testSpec(t))

	if st.Submitted() {
		t.Error("初始状态不应是已提交")
	}
	if st.Product() != nil {
		t.Error("未提交时产物应为 nil")
	}

	if _, err := st.Execute(context.Background(), `{"value":"渗透率 35%"}`); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if !st.Submitted() {
		t.Fatal("提交后 Submitted() 应为 true")
	}

	got, ok := st.Product().(schema.Research)
	if !ok || got.Topic != "渗透率 35%" {
		t.Errorf("产物不对: %+v", st.Product())
	}
}

// TestSubmitToolOverwritesInsteadOfAccumulating 守住 L1 的教训。
//
// 累加会让同一条结论被算两遍（指标虚高、循环永不收敛）；
// 覆盖才是"重新提交一份结论"的正确语义。
func TestSubmitToolOverwritesInsteadOfAccumulating(t *testing.T) {
	st, _ := NewSubmitTool(testSpec(t))
	ctx := context.Background()

	if _, err := st.Execute(ctx, `{"value":"第一版"}`); err != nil {
		t.Fatalf("首次提交失败: %v", err)
	}
	if _, err := st.Execute(ctx, `{"value":"第二版"}`); err != nil {
		t.Fatalf("二次提交失败: %v", err)
	}

	got := st.Product().(schema.Research)
	if got.Topic != "第二版" {
		t.Errorf("应保留最新结论，实际 %q", got.Topic)
	}
}

func TestSubmitToolRejectsInvalidSubmission(t *testing.T) {
	st, _ := NewSubmitTool(testSpec(t))

	if _, err := st.Execute(context.Background(), `{"value":"  "}`); err == nil {
		t.Fatal("校验失败必须返回 error（走回灌自愈）")
	}
	if st.Submitted() {
		t.Error("非法提交不应被记录为'已提交'")
	}

	if _, err := st.Execute(context.Background(), `{"value":`); err == nil {
		t.Fatal("非法 JSON 必须返回 error")
	}
}

func TestSubmitToolDefinitionStatesSingleCall(t *testing.T) {
	st, _ := NewSubmitTool(testSpec(t))
	def := st.Definition()

	if def.Name != "submit_thing" {
		t.Errorf("工具名不对: %q", def.Name)
	}
	if !strings.Contains(def.Description, "替换") {
		t.Errorf("必须写明'重复调用会替换而非累加'：%q", def.Description)
	}
	if len(def.Parameters) != 1 {
		t.Errorf("参数声明不应被改动: %+v", def.Parameters)
	}
}
