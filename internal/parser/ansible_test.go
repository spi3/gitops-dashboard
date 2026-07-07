package parser

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseAnsibleHostsYAMLInventory(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "hosts.yml")
	if err := os.WriteFile(path, []byte(`
all:
  hosts:
    gateway:
      ansible_host: 10.0.0.1
  children:
    docker:
      hosts:
        serenity:
          ansible_host: serenity.lan
        hd3-docker:
    kube:
      children:
        workers:
          hosts:
            kube-worker:
              ansible_host: 10.0.0.12
  vars:
    ansible_user: kube
`), 0o600); err != nil {
		t.Fatal(err)
	}

	hosts, err := ParseAnsibleHosts(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []AnsibleHost{
		{Name: "gateway", Address: "10.0.0.1"},
		{Name: "hd3-docker", Address: "hd3-docker"},
		{Name: "kube-worker", Address: "10.0.0.12"},
		{Name: "serenity", Address: "serenity.lan"},
	}
	if len(hosts) != len(want) {
		t.Fatalf("hosts = %#v, want %#v", hosts, want)
	}
	for i := range want {
		if hosts[i] != want[i] {
			t.Fatalf("hosts[%d] = %#v, want %#v", i, hosts[i], want[i])
		}
	}
}
