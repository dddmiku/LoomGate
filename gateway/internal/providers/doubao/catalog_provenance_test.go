package doubao
import("context";"encoding/json";"strings";"testing")
type reviewBoundRuntime struct{calls int}
func(r *reviewBoundRuntime) Catalog(context.Context,RuntimeBinding,string)(json.RawMessage,error){r.calls++;return json.RawMessage(`{"entry_list":[{"active_switch_conf":{"menu_conf_v2":{"use_mode_model_list":false,"model_list":{"item_list":[{"model_item_key":"synthetic-text","name":"Synthetic text","show_task_tools":0}]}}}}]}`),nil}
func TestReviewRuntimeCatalogProvenanceSurvivesFreshAndCachedCopy(t *testing.T){
 runtime:=&reviewBoundRuntime{}
 sessions:=sessionSourceFunc(func(context.Context,string)(Session,error){return Session{Runtime:&RuntimeBinding{Identity:strings.Repeat("a",64)},BotID:"synthetic-bot",Revision:"r1"},nil})
 client:=NewWithRuntime(sessions,runtime)
 for _,phase:=range []string{"fresh","cached"}{
  got,err:=client.Catalog(context.Background(),"synthetic-account",false)
  if err!=nil{t.Fatal(err)}
  if len(got.Models)!=1{t.Fatal("catalog missing")}
  if !got.RuntimeBound{t.Errorf("%s catalog lost RuntimeBound before real providercatalog routing/InferenceAvailable consumes it",phase)}
 }
 if runtime.calls!=1{t.Fatal("cache not exercised")}
}
