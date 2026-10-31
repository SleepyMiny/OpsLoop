package controller
import("encoding/json";"strings";"testing";"time")
func TestBGrantWireRepresentation(t *testing.T){
 f:=newExhibitionFixture(t)
 f.permit.ModelBudget=&ExhibitionModelBudget{Model:"gpt-6-luna",ReasoningEffort:"medium",AuthMode:"chatgpt",MaxModelCalls:1,TotalSeconds:900,ExpiresAt:f.n.wall.Add(900*time.Second)}
 raw,_:=json.Marshal(f.permit)
 // Same instant, but Python offset form versus the Go wire UTC form.
 f.publishRaw(t,[]byte(strings.ReplaceAll(string(raw),"Z\"","+00:00\"")))
 if err:=f.a.Authorize(f.p,nil);err==nil||err.Error()!="exhibition permit: grant core changed"{t.Fatalf("offset wire mismatch: %v",err)}
 // New fixture permit normalized BEFORE signing. No security checks changed.
 f.publishRaw(t,raw)
 if err:=f.a.Authorize(f.p,nil);err!=nil{t.Fatalf("canonical UTC wire rejected: %v",err)}
 f.grant.Scope.Epoch="foreign";f.publish(t)
 if err:=f.a.Authorize(f.p,nil);err==nil{t.Fatal("foreign epoch accepted")}
}
