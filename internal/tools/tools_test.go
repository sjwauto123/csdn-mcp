package tools

import (
	"reflect"
	"testing"
)

func TestSliceArgMixedCommas(t *testing.T) {
	args := map[string]any{"tags": "Go, MCP，K8s,  ，Go"}
	got := sliceArg(args, "tags")
	want := []string{"Go", "MCP", "K8s"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sliceArg 中文逗号/去重/去空处理错误:\n got=%v\nwant=%v", got, want)
	}
}

func TestSliceArgEmpty(t *testing.T) {
	if got := sliceArg(map[string]any{}, "tags"); got != nil {
		t.Errorf("空输入应返回 nil，却返回 %v", got)
	}
	if got := sliceArg(map[string]any{"tags": ""}, "tags"); got != nil {
		t.Errorf("空串输入应返回 nil，却返回 %v", got)
	}
}
