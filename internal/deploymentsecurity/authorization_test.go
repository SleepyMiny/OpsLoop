package deploymentsecurity

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/SleepyMiny/OpsLoop/internal/testenv"
)

func TestMain(m *testing.M) { testenv.Main(m) }

// Read the actual RBAC kustomization files without shelling out to kubectl or
// contacting any cluster. This subset intentionally needs no transformers.
func resources(t *testing.T, dir string) []client.Object {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var k struct {
		Resources []string `json:"resources"`
	}
	if err = yaml.Unmarshal(b, &k); err != nil {
		t.Fatal(err)
	}
	var out []client.Object
	for _, name := range k.Resources {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.IsDir() {
			out = append(out, resources(t, path)...)
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		dec := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
		for {
			var raw json.RawMessage
			err = dec.Decode(&raw)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(raw) == 0 || string(raw) == "null" {
				continue
			}
			obj, _, err := clientgoscheme.Codecs.UniversalDeserializer().Decode(raw, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			co, ok := obj.(client.Object)
			if !ok {
				t.Fatalf("not a Kubernetes object %T", obj)
			}
			out = append(out, co)
		}
		f.Close()
	}
	return out
}

func TestEnforceWriterIdentityBoundary(t *testing.T) {
	env := testenv.Start(t, filepath.Join("..", "..", "config", "crd", "bases"))
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	admin, err := client.New(env.Config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"opsloop-system", "opsloop-demo", "other-demo"} {
		must(admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}))
	}
	for _, obj := range append(resources(t, "../../config/rbac"), resources(t, "../../config/rbac/enforce")...) {
		must(admin.Create(ctx, obj))
	}
	identity := func(name string) client.Client {
		t.Helper()
		user, err := env.AddUser(envtest.User{Name: "system:serviceaccount:opsloop-system:" + name, Groups: []string{"system:serviceaccounts", "system:serviceaccounts:opsloop-system"}}, nil)
		must(err)
		c, err := client.New(user.Config(), client.Options{Scheme: scheme})
		must(err)
		return c
	}
	old := identity("opsloop-controller")
	fenced := identity("opsloop-controller-fenced-v1")
	// Bound authorization propagation. These are local envtest requests only.
	allowed := func(t *testing.T, c client.Client, verb, name, ns string, want bool) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for {
			review := &authv1.SelfSubjectAccessReview{Spec: authv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authv1.ResourceAttributes{Group: "apps", Resource: "deployments", Verb: verb, Name: name, Namespace: ns}}}
			err := c.Create(ctx, review)
			if err == nil && review.Status.Allowed == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("authorization %s %s/%s want allowed=%v got=%v err=%v", verb, ns, name, want, review.Status.Allowed, err)
			}
			time.Sleep(40 * time.Millisecond)
		}
	}
	one := int32(1)
	makeDeployment := func(name, ns string) *appsv1.Deployment {
		d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: appsv1.DeploymentSpec{Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "example.invalid/app:fixture"}}}}}}
		must(admin.Create(ctx, d))
		return d
	}
	target := makeDeployment("demo-api", "opsloop-demo")
	other := makeDeployment("other", "opsloop-demo")
	cross := makeDeployment("demo-api", "other-demo")
	// A legacy binary ignores the fence and obtains a current RV. Identity, not
	// an old RV, must make this write fail.
	target.Annotations = map[string]string{"opsloop.io/execution-fence": "{\"version\":1,\"policyName\":\"demo-api\",\"policyUID\":\"test\",\"deploymentUID\":\"" + string(target.UID) + "\",\"closedThroughGeneration\":7}"}
	must(admin.Update(ctx, target))
	fresh := func(key client.ObjectKey) *appsv1.Deployment {
		d := &appsv1.Deployment{}
		must(admin.Get(ctx, key, d))
		return d
	}
	update := func(c client.Client, key client.ObjectKey) error {
		d := fresh(key)
		d.Spec.Template.Annotations = map[string]string{"opsloop.io/attempt-id": "legacy-ignores-fence"}
		return c.Update(ctx, d)
	}
	t.Run("old_identity_fresh_rv_update_forbidden", func(t *testing.T) {
		allowed(t, old, "update", target.Name, target.Namespace, false)
		if err := update(old, client.ObjectKeyFromObject(target)); !apierrors.IsForbidden(err) {
			t.Fatalf("legacy fresh-RV update accepted or wrong failure: %v", err)
		}
		t.Log("old identity fresh-RV workload update: API Forbidden")
	})
	t.Run("new_identity_only_named_update", func(t *testing.T) {
		allowed(t, fenced, "update", target.Name, target.Namespace, true)
		must(update(fenced, client.ObjectKeyFromObject(target)))
		for _, d := range []*appsv1.Deployment{other, cross} {
			if err := update(fenced, client.ObjectKeyFromObject(d)); !apierrors.IsForbidden(err) {
				t.Fatalf("out-of-scope update: %v", err)
			}
		}
		patch := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"bypass":"true"}}}`))
		if err := fenced.Patch(ctx, fresh(client.ObjectKeyFromObject(target)), patch); !apierrors.IsForbidden(err) {
			t.Fatalf("patch authority leaked: %v", err)
		}
		if err := fenced.Delete(ctx, fresh(client.ObjectKeyFromObject(target))); !apierrors.IsForbidden(err) {
			t.Fatalf("delete authority leaked: %v", err)
		}
		var secret corev1.Secret
		if err := fenced.Get(ctx, client.ObjectKey{Namespace: "opsloop-demo", Name: "acr-pull"}, &secret); !apierrors.IsForbidden(err) {
			t.Fatalf("secret-read authority leaked: %v", err)
		}
		t.Log("new identity: named update allowed; other target/namespace, patch/delete/secret read Forbidden")
	})
	t.Run("revocation_is_observed_not_assumed", func(t *testing.T) {
		binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "fixture-legacy-grant", Namespace: "opsloop-demo"}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "opsloop-controller-enforce"}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "opsloop-controller", Namespace: "opsloop-system"}}}
		must(admin.Create(ctx, binding))
		allowed(t, old, "update", target.Name, target.Namespace, true)
		must(update(old, client.ObjectKeyFromObject(target)))
		must(admin.Delete(ctx, binding))
		allowed(t, old, "update", target.Name, target.Namespace, false)
		if err := update(old, client.ObjectKeyFromObject(target)); !apierrors.IsForbidden(err) {
			t.Fatalf("revoked legacy writer accepted: %v", err)
		}
		allowed(t, fenced, "update", target.Name, target.Namespace, true)
		t.Log("additive old grant allowed update; after removal and observed revocation old fresh-RV update Forbidden")
	})
}
