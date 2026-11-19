package setup

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"

	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/manifests"
	"github.com/sebrandon1/vsno/lib/pki"
	"github.com/sebrandon1/vsno/lib/precheck"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	registryUser = "vsno"
	redacted     = "<generated>"
)

func (r *runner) namespace(ctx context.Context, _ precheck.ProgressStep) (Step, error) {
	detail, err := r.applyTemplate(ctx, manifests.Namespace, nil)
	if err != nil {
		return Step{Name: manifests.StepNamespace}, err
	}
	r.nsSet = !r.o.DryRun
	return Step{Name: manifests.StepNamespace, Outcome: r.outcome(), Detail: detail}, nil
}

// support applies dnsmasq, builder, S3 and the registry, reusing generated
// credentials and the registry certificate when they are still valid, then
// waits for the Deployments.
func (r *runner) support(ctx context.Context, ps precheck.ProgressStep) (Step, error) {
	step := Step{Name: manifests.StepSupport}
	start := len(r.res.Changes)
	applyAll := func(names ...string) error {
		for _, n := range names {
			if _, err := r.applyTemplate(ctx, n, nil); err != nil {
				return err
			}
		}
		return nil
	}
	if err := r.sshKey(ctx); err != nil {
		return step, err
	}
	if err := applyAll(manifests.DNSMasq, manifests.Builder, manifests.RegistryService); err != nil {
		return step, err
	}
	if err := r.credentials(ctx); err != nil {
		return step, err
	}
	if err := applyAll(manifests.S3, manifests.Registry); err != nil {
		return step, err
	}
	step.Outcome, step.Detail = r.outcome(), r.detail(r.res.Changes[start:])
	if r.o.DryRun {
		return step, nil
	}
	if err := r.waitDeployments(ctx, ps); err != nil {
		return step, err
	}
	for _, n := range cluster.Deployments {
		ip, err := r.clusterIP(ctx, n)
		if err != nil {
			return step, err
		}
		r.st.Services[n] = ip
	}
	r.res.Registry = net.JoinHostPort(r.st.Services["registry"], fmt.Sprint(cluster.RegistryPort))
	return step, nil
}

// sshKey fills the builder's SSH key pair into r.cfg, reusing the stored one.
func (r *runner) sshKey(ctx context.Context) error {
	if r.o.DryRun {
		r.cfg.SSHPrivateKey, r.cfg.SSHPublicKey = redacted, redacted
		return nil
	}
	s, err := r.secret(ctx, "vsno-ssh")
	if err != nil {
		return err
	}
	if pki.SSHKeyMatches(s["id_ed25519"], s["id_ed25519.pub"]) {
		r.cfg.SSHPrivateKey, r.cfg.SSHPublicKey = s["id_ed25519"], s["id_ed25519.pub"]
		return nil
	}
	r.cfg.SSHPrivateKey, r.cfg.SSHPublicKey, err = pki.SSHKey("vsno@" + r.o.Namespace)
	return err
}

// credentials fills the S3 keys and registry TLS/auth into r.cfg, reusing
// what earlier runs stored in the namespace's Secrets.
func (r *runner) credentials(ctx context.Context) error {
	if r.o.DryRun {
		r.cfg.S3AccessKey, r.cfg.S3SecretKey = redacted, redacted
		r.cfg.Registry = manifests.RegistryAuth{CACert: redacted, CAKey: redacted, Cert: redacted, Key: redacted,
			User: registryUser, Password: redacted, Htpasswd: redacted}
		return nil
	}
	s3, err := r.secret(ctx, "s3-config")
	if err != nil {
		return err
	}
	if r.cfg.S3AccessKey, r.cfg.S3SecretKey = s3["accessKey"], s3["secretKey"]; r.cfg.S3AccessKey == "" || r.cfg.S3SecretKey == "" {
		if r.cfg.S3AccessKey, err = pki.RandomHex(10); err != nil {
			return err
		}
		if r.cfg.S3SecretKey, err = pki.RandomHex(20); err != nil {
			return err
		}
	}
	ip, err := r.clusterIP(ctx, "registry")
	if err != nil {
		return err
	}
	if err := r.registryTLS(ctx, ip); err != nil {
		return err
	}
	return r.registryAuth(ctx)
}

// registryTLS reuses the CA, and the server cert if it still covers the
// registry's ClusterIP and Service names.
func (r *runner) registryTLS(ctx context.Context, ip string) error {
	ca, err := r.secret(ctx, "registry-ca")
	if err != nil {
		return err
	}
	tls, err := r.secret(ctx, "registry-tls")
	if err != nil {
		return err
	}
	a := &r.cfg.Registry
	a.CACert, a.CAKey = ca["ca.crt"], ca["ca.key"]
	now := r.Now()
	if a.CACert == "" || a.CAKey == "" {
		c, k, err := pki.NewCA("vsno registry CA", now)
		if err != nil {
			return err
		}
		a.CACert, a.CAKey = string(c), string(k)
	}
	ips := []net.IP{net.ParseIP(ip)}
	names := []string{"registry." + r.o.Namespace + ".svc", "registry." + r.o.Namespace + ".svc.cluster.local"}
	if pki.Covers([]byte(tls["tls.crt"]), []byte(a.CACert), ips, names, now) {
		a.Cert, a.Key = tls["tls.crt"], tls["tls.key"]
		return nil
	}
	c, k, err := pki.IssueServer([]byte(a.CACert), []byte(a.CAKey), ips, names, now)
	if err != nil {
		return fmt.Errorf("issuing the registry certificate: %w", err)
	}
	a.Cert, a.Key = string(c), string(k)
	return nil
}

// registryAuth reuses the stored credentials if the htpasswd still matches.
func (r *runner) registryAuth(ctx context.Context) error {
	auth, err := r.secret(ctx, "registry-auth")
	if err != nil {
		return err
	}
	a := &r.cfg.Registry
	a.User, a.Password, a.Htpasswd = auth["username"], auth["password"], auth["htpasswd"]
	if a.User != "" && pki.CheckHtpasswd(a.Htpasswd, a.User, a.Password) {
		return nil
	}
	a.User = registryUser
	if a.Password, err = pki.RandomHex(16); err != nil {
		return err
	}
	a.Htpasswd, err = pki.Htpasswd(a.User, a.Password)
	return err
}

// secret returns a Secret's decoded data, or nil if it doesn't exist.
func (r *runner) secret(ctx context.Context, name string) (map[string]string, error) {
	u, err := r.Dyn.Resource(cluster.GVRSecret).Namespace(r.o.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading secret %s: %w", name, err)
	}
	data, _, _ := unstructured.NestedStringMap(u.Object, "data")
	out := make(map[string]string, len(data))
	for k, v := range data {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("secret %s key %s: %w", name, k, err)
		}
		out[k] = string(b)
	}
	return out, nil
}

func (r *runner) clusterIP(ctx context.Context, svc string) (string, error) {
	u, err := r.Dyn.Resource(cluster.GVRService).Namespace(r.o.Namespace).Get(ctx, svc, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading service %s: %w", svc, err)
	}
	ip, _, _ := unstructured.NestedString(u.Object, "spec", "clusterIP")
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("service %s has no ClusterIP (%q)", svc, ip)
	}
	return ip, nil
}

// waitDeployments waits until every support Deployment has rolled out.
func (r *runner) waitDeployments(ctx context.Context, ps precheck.ProgressStep) error {
	deps := r.Dyn.Resource(cluster.GVRDeployment).Namespace(r.o.Namespace)
	return r.wait(ctx, "support Deployments", func(ctx context.Context) (bool, error) {
		ready := 0
		var pending string
		for _, n := range cluster.Deployments {
			d, err := deps.Get(ctx, n, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			if cluster.RolledOut(d) {
				ready++
				continue
			}
			if err := r.crashLooping(ctx, n); err != nil {
				return false, err
			}
			if pending == "" {
				pending = n
			}
		}
		ps.SetProgress(ready, len(cluster.Deployments))
		if pending != "" {
			ps.Update("waiting for " + pending)
		}
		return ready == len(cluster.Deployments), nil
	})
}

// crashLoopRestarts is how many restarts in CrashLoopBackOff end the wait.
const crashLoopRestarts = 3

// crashLooping returns errStuck if a pod of Deployment app keeps crashing.
func (r *runner) crashLooping(ctx context.Context, app string) error {
	pods, err := r.Kube.CoreV1().Pods(r.o.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=" + app})
	if err != nil {
		return err
	}
	for _, p := range pods.Items {
		for _, c := range p.Status.ContainerStatuses {
			if w := c.State.Waiting; w != nil && w.Reason == "CrashLoopBackOff" && c.RestartCount >= crashLoopRestarts {
				return errStuck{fmt.Errorf("pod %s keeps crashing (%d restarts); see `oc logs -n %s %s --previous`", p.Name, c.RestartCount, r.o.Namespace, p.Name)}
			}
		}
	}
	return nil
}
