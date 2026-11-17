package server

import (
 "net/http/httptest"
 "strings"
 "testing"
 "workbuddy2api/internal/inference"
)

func TestProviderSystemInstructionsRejectedBeforeProviderCall(t *testing.T) {
 for _, tc := range []struct{name,path,body string}{
  {"chat", "/v1/chat/completions", `{"model":"doubao:synthetic:model","messages":[{"role":"system","content":"Synthetic system"},{"role":"user","content":"Synthetic user"}]}`},
  {"messages", "/v1/messages", `{"model":"doubao:synthetic:model","max_tokens":64,"system":"Synthetic system","messages":[{"role":"user","content":"Synthetic user"}]}`},
  {"responses", "/v1/responses", `{"model":"doubao:synthetic:model","instructions":"Synthetic system","input":"Synthetic user"}`},
  {"gemini", "/v1beta/models/doubao:synthetic:model:generateContent", `{"systemInstruction":{"parts":[{"text":"Synthetic system"}]},"contents":[{"role":"user","parts":[{"text":"Synthetic user"}]}]}`},
 } {
  t.Run(tc.name,func(t *testing.T){
   p:=&dialogueFixtureProvider{}
   h:=NewHandler(Config{Pool:testPoolWith(),APIKey:"synthetic-key",Inference:map[string]inference.Provider{"doubao":p}})
   req:=httptest.NewRequest("POST",tc.path,strings.NewReader(tc.body));req.Header.Set("Authorization","Bearer synthetic-key");req.Header.Set("Content-Type","application/json")
   w:=httptest.NewRecorder();h.ServeHTTP(w,req)
   if w.Code!=400 {t.Fatalf("system instruction was not rejected: %d",w.Code)}
   if len(p.calls)!=0 {t.Fatal("provider was called despite unsupported system instructions")}
   // /v1/messages converts system to a top-level parameter which this
   // provider rejects generically; chat/responses/gemini reach the explicit
   // instruction guard. Both paths are pre-flight and never call the provider.
   allowed:=[]string{"provider_system_instructions_unsupported","provider_parameter_unsupported","provider_output_budget_unsupported"}
   matched:=false;for _,code:=range allowed {if strings.Contains(w.Body.String(),code) {matched=true}}
   if !matched {t.Fatalf("explicit pre-flight rejection missing: %s",w.Body.String())}
  })
 }
}
