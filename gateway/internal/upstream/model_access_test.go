package upstream

import "testing"

func TestModelAccessDeniedIsNotAnOutputParameterFailure(t *testing.T) {
	for _, body := range []string{
		`{"code":11158,"msg":"model fixture-model is not allowed for this free user"}`,
		`{"code":"11158","msg":"model permission required"}`,
	} {
		if !IsModelAccessDenied(body) {
			t.Fatal("model entitlement rejection was not recognized")
		}
	}
	for _, body := range []string{`{"code":11133,"msg":"model_param_invalid"}`, `model not allowed`, `{"code":11140}`} {
		if IsModelAccessDenied(body) {
			t.Fatal("an unrelated or unstructured error became a model entitlement rejection")
		}
	}
}
