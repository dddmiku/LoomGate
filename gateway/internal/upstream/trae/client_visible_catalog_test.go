package trae

import "testing"

func TestClientDirectoryDoesNotPublishInvisibleOrUtilityOnlyModels(t *testing.T) {
 rows,err:=parseCatalog([]byte(`{"function_configs":[
 {"function":"solo_agent","config_info_list":[{"config_name":"fixture-visible","usage":"chat_completion","is_invisible_to_user":false},{"config_name":"fixture-hidden","usage":"chat_completion","is_invisible_to_user":true}]},
 {"function":"utils","config_info_list":[{"config_name":"fixture-hidden","usage":"chat_completion","is_invisible_to_user":false},{"config_name":"fixture-utils","usage":"chat_completion","is_invisible_to_user":false}]}
 ]}`))
 if err!=nil{t.Fatal(err)}
 ids:=ModelIDsFromCatalog(rows)
 if len(ids)!=1 || ids[0]!="fixture-visible" {t.Fatalf("non-client models published: %v",ids)}
 for _,m:=range rows{if m.ConfigName=="fixture-hidden" && !m.Functions["utils"] {t.Fatal("hidden outbound profile was deleted instead of just hidden")}}
}
