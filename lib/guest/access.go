package guest

import (
	"context"
	"fmt"
	"io"

	"github.com/sebrandon1/vsno/lib/builder"
	"github.com/sebrandon1/vsno/lib/cluster"
	"github.com/sebrandon1/vsno/lib/manifests"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const mergePatch = types.MergePatchType

// lookup returns the Service IP of vSNO name, failing if it doesn't exist.
func lookup(ctx context.Context, e Env, ns, name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	if _, err := e.Dyn.Resource(cluster.GVRVirtualMachine).Namespace(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("no vSNO named %s in %s (see `vsno list`)", name, ns)
		}
		return "", fmt.Errorf("reading vSNO %s: %w", name, err)
	}
	svc, err := e.Kube.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading the Service of vSNO %s: %w", name, err)
	}
	if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == corev1.ClusterIPNone {
		return "", fmt.Errorf("the Service of vSNO %s has no ClusterIP", name)
	}
	return svc.Spec.ClusterIP, nil
}

// Scripts for the access helpers. Each takes the vSNO name and Service IP
// first; the guest's API is reached by its ClusterIP with the certificate's
// name for TLS.
const (
	kubeconfigScript = `cat "/work/guests/$1/build/auth/kubeconfig" 2>/dev/null || { echo "no kubeconfig for $1 yet: its ISO hasn't been built" >&2; exit 1; }`
	ocScript         = `name=$1 ip=$2 base=$3; shift 3
exec oc --kubeconfig "/work/guests/$name/build/auth/kubeconfig" --server "https://$ip:6443" --tls-server-name "api.$name.$base" "$@"`
	sshScript = `ip=$2; shift 3
exec ssh -i /etc/vsno-ssh/id_ed25519 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR "core@$ip" "$@"`
)

// Kubeconfig writes vSNO name's admin kubeconfig to w. Its server,
// api.<name>.vsno.lab, resolves only through the environment's dnsmasq.
func Kubeconfig(ctx context.Context, e Env, ns, name string, w, stderr io.Writer) error {
	if _, err := lookup(ctx, e, ns, name); err != nil {
		return err
	}
	return e.Builder.Run(ctx, kubeconfigScript, []string{name}, builder.IO{Stdout: w, Stderr: stderr})
}

// Oc runs oc with args against vSNO name, through the builder.
func Oc(ctx context.Context, e Env, ns, name string, args []string, s builder.IO) error {
	return run(ctx, e, ns, name, ocScript, args, s)
}

// SSH runs ssh to core@ vSNO name, through the builder; with no args it is
// an interactive shell (s.TTY).
func SSH(ctx context.Context, e Env, ns, name string, args []string, s builder.IO) error {
	return run(ctx, e, ns, name, sshScript, args, s)
}

func run(ctx context.Context, e Env, ns, name, sc string, args []string, s builder.IO) error {
	ip, err := lookup(ctx, e, ns, name)
	if err != nil {
		return err
	}
	return e.Builder.Run(ctx, sc, append([]string{name, ip, manifests.DefaultBaseDomain}, args...), s)
}

// Screenshot returns a PNG of vSNO name's VGA console, from KubeVirt's VNC
// screenshot subresource.
func Screenshot(ctx context.Context, e Env, ns, name string) ([]byte, error) {
	if _, err := lookup(ctx, e, ns, name); err != nil {
		return nil, err
	}
	rc := e.Kube.Discovery().RESTClient()
	if rc == nil {
		return nil, fmt.Errorf("no REST client for the screenshot")
	}
	b, err := rc.Get().AbsPath("/apis/subresources.kubevirt.io/v1/namespaces", ns, "virtualmachineinstances", name, "vnc", "screenshot").DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("taking a screenshot of %s (is it running?): %w", name, err)
	}
	return b, nil
}
