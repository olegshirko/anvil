package main

import (
	"encoding/json"
	"testing"
)

func TestStaticConflistBytesWidensRange(t *testing.T) {
	in := `{"name":"n","plugins":[{"type":"loopback"},{"type":"bridge","ipam":{"type":"host-local",
	"ranges":[[{"subnet":"10.77.0.0/24","gateway":"10.77.0.1","rangeStart":"10.77.0.128","rangeEnd":"10.77.0.254"}]]}}]}`
	out, err := staticConflistBytes([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var conf struct {
		Name    string `json:"name"`
		Plugins []struct {
			IPAM struct {
				Ranges [][]map[string]string `json:"ranges"`
			} `json:"ipam"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(out, &conf); err != nil {
		t.Fatal(err)
	}
	r := conf.Plugins[1].IPAM.Ranges[0][0]
	if conf.Name != "n" || r["subnet"] != "10.77.0.0/24" || r["gateway"] != "10.77.0.1" || r["rangeStart"] != "" || r["rangeEnd"] != "" {
		t.Fatalf("widened conflist: %s", out)
	}
}
