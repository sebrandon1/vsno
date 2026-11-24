package report

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/sebrandon1/vsno/lib/cache"
	"github.com/sebrandon1/vsno/lib/guest"
	"github.com/sebrandon1/vsno/lib/registry"
	"github.com/sebrandon1/vsno/lib/seed"
	"github.com/sebrandon1/vsno/lib/status"
	corev1 "k8s.io/api/core/v1"
)

// The report holds no credentials, by type: a Registry, a Secret or a
// password-bearing struct cannot hide in it.
func TestReportCarriesNoCredentials(t *testing.T) {
	banned := []reflect.Type{
		reflect.TypeOf(registry.Registry{}),
		reflect.TypeOf(corev1.Secret{}),
		reflect.TypeOf(cache.Env{}),
	}
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type) bool
	walk = func(t reflect.Type) bool {
		if seen[t] {
			return false
		}
		seen[t] = true
		for _, b := range banned {
			if t == b {
				return true
			}
		}
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			return walk(t.Elem())
		case reflect.Map:
			return walk(t.Elem())
		case reflect.Struct:
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				if f.Type == reflect.TypeOf(registry.Registry{}) || f.Name == "Password" || f.Name == "SecretKey" {
					return true
				}
				if walk(f.Type) {
					return true
				}
			}
		}
		return false
	}
	if walk(reflect.TypeOf(Report{})) {
		t.Fatal("Report's type graph reaches a credentials type")
	}
}

// Credential-shaped values never reach the marshaled document.
func TestReportMarshalHasNoSecrets(t *testing.T) {
	rep := &Report{
		Namespace: "vsno",
		Status:    &status.Report{Namespace: "vsno", Exists: true, Registry: "registry.vsno.svc:5000"},
		VSNOs:     []guest.Guest{{Name: "a", State: guest.Ready, IBU: "Upgrade done"}},
		Cache:     &cache.Report{Used: 1, Total: 2, Entries: []cache.Entry{{Version: "4.22.22", Repo: "ocp/release/aa"}}},
		Seeds:     []seed.Image{{Ref: "reg/seeds/a:4.22.22", From: "a", Tag: "4.22.22"}},
	}
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"password", "auths", "dockerconfigjson", "ca.crt", "secretKey", "accessKey"} {
		if strings.Contains(strings.ToLower(string(b)), bad) {
			t.Errorf("the document holds %q:\n%s", bad, b)
		}
	}
	if !strings.Contains(string(b), `"namespace":"vsno"`) {
		t.Errorf("the document lost the namespace:\n%s", b)
	}
}
