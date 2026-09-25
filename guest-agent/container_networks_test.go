package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestSecondaryNetworksFromCreate(t *testing.T) {
	req := dockerCreateRequest{
		HostConfig: dockerHostConfig{NetworkMode: "proj_front"},
		NetworkingConfig: &dockerNetworkingConf{EndpointsConfig: map[string]dockerEndpoint{
			"proj_front": {}, "proj_back": {}, "proj_db": {}, "host": {},
		}},
	}
	if got := secondaryNetworksFromCreate(req); !reflect.DeepEqual(got, []string{"proj_back", "proj_db"}) {
		t.Errorf("secondaries = %v", got)
	}
	req.HostConfig.NetworkMode = "host"
	if got := secondaryNetworksFromCreate(req); got != nil {
		t.Errorf("host network got secondaries %v", got)
	}
	// docker run --network a: the only endpoint is the primary.
	one := dockerCreateRequest{
		HostConfig:       dockerHostConfig{NetworkMode: "a"},
		NetworkingConfig: &dockerNetworkingConf{EndpointsConfig: map[string]dockerEndpoint{"a": {}}},
	}
	if got := secondaryNetworksFromCreate(one); got != nil {
		t.Errorf("single network got secondaries %v", got)
	}
}

func TestNextIfName(t *testing.T) {
	if got := nextIfName(nil); got != "eth1" {
		t.Errorf("first = %s", got)
	}
	// A disconnected eth1 is reused before eth3.
	if got := nextIfName([]netEndpoint{{IfName: "eth2"}}); got != "eth1" {
		t.Errorf("gap = %s", got)
	}
	if got := nextIfName([]netEndpoint{{IfName: "eth1"}, {IfName: "eth2"}}); got != "eth3" {
		t.Errorf("next = %s", got)
	}
}

func TestNetInfoEndpoints(t *testing.T) {
	ni := containerNetInfo{IP: "10.10.1.2", Network: "front",
		Extra: []netEndpoint{{Network: "back", IfName: "eth1", IP: "10.10.2.3", Mac: "aa"}}}
	if ni.ipOn("front") != "10.10.1.2" || ni.ipOn("back") != "10.10.2.3" || ni.ipOn("other") != "" {
		t.Errorf("ipOn: %q %q %q", ni.ipOn("front"), ni.ipOn("back"), ni.ipOn("other"))
	}
	if ep, ok := ni.endpointOn("back"); !ok || ep.IPAddress != "10.10.2.3" || ep.MacAddress != "aa" {
		t.Errorf("endpointOn(back) = %+v %v", ep, ok)
	}
}

// A container on two networks sees the peers of both, each at its address
// on the shared network; a single-network peer sees only its own network.
func TestHostsBlockUnionAcrossNetworks(t *testing.T) {
	api := &containerMeta{Namespace: "p", ID: "api", Name: "api", Networks: []string{"front", "back"}}
	web := &containerMeta{Namespace: "p", ID: "web", Name: "web", Networks: []string{"front"}}
	db := &containerMeta{Namespace: "p", ID: "db", Name: "db", Aliases: []string{"postgres"}, Networks: []string{"back"}}
	infos := map[string]containerNetInfo{
		"api": {Network: "front", IP: "10.1.0.2", Extra: []netEndpoint{{Network: "back", IfName: "eth1", IP: "10.2.0.2"}}},
		"web": {Network: "front", IP: "10.1.0.3"},
		"db":  {Network: "back", IP: "10.2.0.4"},
	}
	entries := networkHostsEntries([]*containerMeta{api, web, db}, func(_, id string) (containerNetInfo, bool) {
		ni, ok := infos[id]
		return ni, ok
	})

	apiBlock := hostsBlockFor(api, entries)
	for _, want := range []string{"10.1.0.3\tweb", "10.2.0.4\tdb postgres", "10.1.0.2\tapi", "10.2.0.2\tapi"} {
		if !strings.Contains(apiBlock, want) {
			t.Errorf("api block missing %q:\n%s", want, apiBlock)
		}
	}
	webBlock := hostsBlockFor(web, entries)
	if strings.Contains(webBlock, "db") || strings.Contains(webBlock, "10.2.0.2") {
		t.Errorf("web sees the back network:\n%s", webBlock)
	}
	if !strings.Contains(webBlock, "10.1.0.2\tapi") {
		t.Errorf("web does not see api on front:\n%s", webBlock)
	}
}
