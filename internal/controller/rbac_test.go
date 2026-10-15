package controller

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

var readVerbs = []string{"get", "list", "watch"}

// deploymentWriteRules returns every rule that grants Deployments more than read access, including wildcards.
func deploymentWriteRules(rules []rbacv1.PolicyRule) []rbacv1.PolicyRule {
	var bad []rbacv1.PolicyRule
	for _, r := range rules {
		apps := slices.Contains(r.APIGroups, "apps") || slices.Contains(r.APIGroups, "*")
		deploy := slices.ContainsFunc(r.Resources, func(s string) bool {
			return s == "*" || s == "deployments" || strings.HasPrefix(s, "deployments/")
		})
		if !apps || !deploy {
			continue
		}
		if slices.ContainsFunc(r.Verbs, func(v string) bool { return !slices.Contains(readVerbs, v) }) {
			bad = append(bad, r)
		}
	}
	return bad
}

func decodeFile(t *testing.T, path string, into any) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := yaml.NewYAMLOrJSONDecoder(f, 4096).Decode(into); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

func TestDeploymentWriteRulesDetectsBypasses(t *testing.T) {
	rule := func(groups, resources, verbs []string) rbacv1.PolicyRule {
		return rbacv1.PolicyRule{APIGroups: groups, Resources: resources, Verbs: verbs}
	}
	cases := []struct {
		name string
		rule rbacv1.PolicyRule
		bad  bool
	}{
		{"read only", rule([]string{"apps"}, []string{"deployments"}, readVerbs), false},
		{"update", rule([]string{"apps"}, []string{"deployments"}, []string{"get", "update"}), true},
		{"patch", rule([]string{"apps"}, []string{"deployments", "replicasets"}, []string{"patch"}), true},
		{"wildcard verb", rule([]string{"apps"}, []string{"deployments"}, []string{"*"}), true},
		{"wildcard group", rule([]string{"*"}, []string{"deployments"}, []string{"update"}), true},
		{"wildcard resource", rule([]string{"apps"}, []string{"*"}, []string{"delete"}), true},
		{"subresource", rule([]string{"apps"}, []string{"deployments/scale"}, []string{"update"}), true},
		{"other group", rule([]string{"opsloop.io"}, []string{"recoveryincidents"}, []string{"create"}), false},
	}
	for _, tc := range cases {
		if got := len(deploymentWriteRules([]rbacv1.PolicyRule{tc.rule})) > 0; got != tc.bad {
			t.Errorf("%s: flagged=%v, want %v", tc.name, got, tc.bad)
		}
	}
}

func TestGeneratedClusterRoleReadsDeploymentsOnly(t *testing.T) {
	var cr rbacv1.ClusterRole
	decodeFile(t, filepath.Join("..", "..", "config", "rbac", "role.yaml"), &cr)
	if cr.Kind != "ClusterRole" || cr.Name != "opsloop-controller" {
		t.Fatalf("unexpected role %s/%s", cr.Kind, cr.Name)
	}
	if len(cr.Rules) == 0 {
		t.Fatal("ClusterRole has no rules")
	}
	if bad := deploymentWriteRules(cr.Rules); len(bad) > 0 {
		t.Fatalf("ClusterRole grants Deployment writes: %+v", bad)
	}
}

func TestEnforceRoleLimitedToDemoAPIUpdate(t *testing.T) {
	var role rbacv1.Role
	decodeFile(t, filepath.Join("..", "..", "config", "rbac", "enforce", "role.yaml"), &role)
	if role.Kind != "Role" || role.Namespace != "opsloop-demo" {
		t.Fatalf("enforce permissions must be a Role in opsloop-demo, got %s in %q", role.Kind, role.Namespace)
	}
	if len(role.Rules) != 1 {
		t.Fatalf("enforce Role must have exactly one rule, got %d", len(role.Rules))
	}
	r := role.Rules[0]
	if !slices.Equal(r.APIGroups, []string{"apps"}) || !slices.Equal(r.Resources, []string{"deployments"}) ||
		!slices.Equal(r.ResourceNames, []string{"demo-api"}) || !slices.Equal(r.Verbs, []string{"update"}) {
		t.Fatalf("enforce rule must be apps/deployments resourceNames [demo-api] verbs [update], got %+v", r)
	}
}
