//go:build ignore

package main
import("net/http";"fmt")
func main(){http.HandleFunc("/",func(w http.ResponseWriter,r *http.Request){fmt.Fprintln(w,"static-arm64-ok")});panic(http.ListenAndServe(":8080",nil))}
