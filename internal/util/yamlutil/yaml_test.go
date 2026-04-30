package yamlutil

import (
	"net"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"

	"anvil/internal/domain"
)

func TestConfigMarshaling(t *testing.T) {
	input := domain.Config{
		Docker:     map[string]any{"insecure-registries": []any{"127.0.0.1"}},
		Network:    domain.Network{DNSResolvers: []net.IP{net.ParseIP("1.1.1.1")}},
		Kubernetes: domain.Kubernetes{K3sArgs: []string{"--disable=traefik"}},
	}

	cases := []struct {
		label   string
		cfg     domain.Config
		expect  domain.Config
		wantErr bool
	}{
		{label: "nested-fields", cfg: input, expect: input},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			b, err := marshalConfig(tc.cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("marshalConfig() error = %v, wantErr %v", err, tc.wantErr)
			}

			var decoded domain.Config
			if err := yaml.Unmarshal(b, &decoded); err != nil {
				t.Fatalf("decoded bytes are invalid yaml: %v", err)
			}

			if !reflect.DeepEqual(decoded.Docker, tc.expect.Docker) {
				t.Errorf("Docker mismatch:\ngot  %+v\nwant %+v", decoded.Docker, tc.expect.Docker)
			}
		})
	}
}
