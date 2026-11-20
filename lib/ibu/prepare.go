package ibu

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"net"
	"strconv"
	"text/template"
	"time"

	"github.com/sebrandon1/vsno/lib/guestctl"
	"github.com/sebrandon1/vsno/lib/manifests"
	"github.com/sebrandon1/vsno/lib/precheck"
	"github.com/sebrandon1/vsno/lib/seed"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Role is what a vSNO is prepared for.
type Role string

const (
	// RoleSeed makes seed images: it needs LCA.
	RoleSeed Role = "seed"
	// RoleTarget is upgraded: it also needs OADP with a backup location, and
	// the credentials to pull the seed image.
	RoleTarget Role = "target"
)

// OADP is the OpenShift API for Data Protection, which LCA backs the target's
// applications up with before the upgrade and restores them from after.
var OADP = guestctl.Operator{Name: "OADP", Package: "redhat-oadp-operator", CSVPrefix: "oadp-operator.", Namespace: oadpNamespace}

const (
	oadpNamespace = "openshift-adp"
	dpaName       = "dataprotectionapplication"
	// PullSecret is the Secret in LCA's namespace the target pulls the seed
	// image with.
	PullSecret = "seed-pull-secret"

	operatorWait = 10 * time.Minute
	backupWait   = 5 * time.Minute
)

// PrepareOptions are the user's choices for vsno prepare.
type PrepareOptions struct {
	Namespace string
	Names     []string
	Role      Role
	// LCA is where LCA comes from; the zero value is the vSNO's catalog.
	LCA LCASource
}

// Prepared is what Prepare left on one vSNO.
type Prepared struct {
	Name string `json:"name"`
	Role Role   `json:"role"`
	LCA  string `json:"lca"` // its CSV, or the image built from a ref
	OADP string `json:"oadp"`
	// Built is the LCA built from a ref and deployed, when not from the catalog.
	Built    *BuiltLCA `json:"built,omitempty"`
	Warnings []string  `json:"warnings,omitempty"`
}

// Prepare readies each vSNO for its role, reusing whatever is already there.
func Prepare(ctx context.Context, e Env, o PrepareOptions) ([]Prepared, error) {
	e.Defaults(o.Namespace)
	reg, err := seed.LoadRegistry(ctx, e.Kube, o.Namespace)
	if err != nil {
		return nil, err
	}
	var store *backupStore
	if o.Role == RoleTarget {
		if store, err = loadBackupStore(ctx, e, o.Namespace); err != nil {
			return nil, err
		}
	}
	var built *BuiltLCA
	if !o.LCA.FromCatalog() {
		if err := guestctl.Step(e.Progress, "Building LCA", func(ps precheck.ProgressStep) (string, error) {
			built, err = buildLCA(ctx, e, o.Namespace, o.LCA, reg, ps)
			if err != nil {
				return "", err
			}
			return built.String(), nil
		}); err != nil {
			return nil, err
		}
	}
	out := make([]Prepared, 0, len(o.Names))
	for _, name := range o.Names {
		p, err := prepareOne(ctx, e, o.Namespace, name, o.Role, reg, store, built)
		out = append(out, p)
		if err != nil {
			return out, fmt.Errorf("%s: %w", name, err)
		}
	}
	return out, nil
}

func prepareOne(ctx context.Context, e Env, ns, name string, role Role, reg seed.Registry, store *backupStore, built *BuiltLCA) (Prepared, error) {
	p := Prepared{Name: name, Role: role, Built: built}
	a := e.Access(name)
	c := e.Clock()
	if err := guestctl.Step(e.Progress, name+": LCA", func(ps precheck.ProgressStep) (string, error) {
		if built == nil {
			// A vSNO running LCA from a ref keeps it: subscribing to the
			// catalog's would replace it.
			if image, fromSource, err := seed.SourceLCA(ctx, a); err != nil || fromSource {
				p.LCA = image
				return image + " (from source, kept)", err
			}
			var err error
			p.LCA, err = seed.LCA.Ensure(ctx, a, c, operatorWait, ps)
			return p.LCA, err
		}
		p.LCA = built.Image
		p.Warnings = append(p.Warnings, versionWarning(ctx, a, built)...)
		if err := deployLCA(ctx, a, c, built, reg, ps); err != nil {
			return "", err
		}
		return built.String(), recordLCA(ctx, e, ns, name, built)
	}); err != nil {
		return p, err
	}
	if err := guestctl.Step(e.Progress, name+": "+OADP.Name, func(ps precheck.ProgressStep) (string, error) {
		var err error
		p.OADP, err = OADP.Ensure(ctx, a, c, operatorWait, ps)
		return p.OADP, err
	}); err != nil {
		return p, err
	}
	if err := guestctl.Step(e.Progress, name+": trusting the environment registry", func(ps precheck.ProgressStep) (string, error) {
		return reg.Host, seed.EnsureTrust(ctx, e, a, reg, ps)
	}); err != nil {
		return p, err
	}
	if role != RoleTarget {
		return p, nil
	}
	if err := guestctl.Step(e.Progress, name+": backup location", func(ps precheck.ProgressStep) (string, error) {
		return store.url, ensureBackupLocation(ctx, a, c, store, ps)
	}); err != nil {
		return p, err
	}
	return p, guestctl.Step(e.Progress, name+": seed pull secret", func(precheck.ProgressStep) (string, error) {
		y, err := render("seed-pull-secret.yaml.tmpl", data{Auth: reg.Auth()})
		if err != nil {
			return "", err
		}
		return seed.LCANamespace + "/" + PullSecret, guestctl.Apply(ctx, a, y)
	})
}

// versionWarning warns when b's version isn't the vSNO's OpenShift minor:
// LCA releases follow OpenShift's, and a mismatch is rarely what was meant.
func versionWarning(ctx context.Context, a guestctl.Access, b *BuiltLCA) []string {
	id, err := identity(ctx, a)
	if err != nil || b.Version == "" || minorOf(b.Version) == minorOf(id.Version) {
		return nil
	}
	return []string{fmt.Sprintf("LCA %s at %s is version %s, but the vSNO runs OpenShift %s", b.Ref, b.SHA, b.Version, id.Version)}
}

// backupStore is the environment's S3, where the target's DPA keeps backups.
type backupStore struct {
	url, accessKey, secretKey string
}

// loadBackupStore reads the S3 Service's address and the keys setup made. The
// guest reaches it by ClusterIP: its own cluster.local hides the host's names.
func loadBackupStore(ctx context.Context, e Env, ns string) (*backupStore, error) {
	svc, err := e.Kube.CoreV1().Services(ns).Get(ctx, "s3", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading the S3 Service (see `vsno status`): %w", err)
	}
	if svc.Spec.ClusterIP == "" || len(svc.Spec.Ports) == 0 {
		return nil, errors.New("the S3 Service has no ClusterIP or port")
	}
	sec, err := e.Kube.CoreV1().Secrets(ns).Get(ctx, "s3-config", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading the S3 keys: %w", err)
	}
	s := &backupStore{
		url:       "http://" + net.JoinHostPort(svc.Spec.ClusterIP, strconv.Itoa(int(svc.Spec.Ports[0].Port))),
		accessKey: string(sec.Data["accessKey"]), secretKey: string(sec.Data["secretKey"]),
	}
	if s.accessKey == "" || s.secretKey == "" {
		return nil, errors.New("secret s3-config has no accessKey or secretKey; rerun `vsno setup`")
	}
	return s, nil
}

// ensureBackupLocation applies the cloud credentials and the DPA, and waits
// for OADP to report the backup location Available.
func ensureBackupLocation(ctx context.Context, a guestctl.Access, c guestctl.Clock, s *backupStore, ps precheck.ProgressStep) error {
	y, err := render("backup.yaml.tmpl", data{AccessKey: s.accessKey, SecretKey: s.secretKey, S3URL: s.url})
	if err != nil {
		return err
	}
	if err := guestctl.Apply(ctx, a, y); err != nil {
		return err
	}
	var phase string
	err = guestctl.Poll(ctx, c, backupWait, func() (bool, error) {
		var bsls guestctl.ObjectList
		if _, err := guestctl.Get(ctx, a, &bsls, "get", "backupstoragelocations", "-n", oadpNamespace); err != nil {
			return false, err
		}
		phase = "not created yet"
		if len(bsls.Items) > 0 {
			phase = bsls.Items[0].Status.Phase
		}
		ps.Update("backup location: " + phase)
		return phase == "Available", nil
	})
	if errors.Is(err, guestctl.ErrTimeout) {
		return fmt.Errorf("the backup location at %s was %s after %s (`vsno oc <name> -- get backupstoragelocations -n %s -o yaml`)", s.url, phase, backupWait, oadpNamespace)
	}
	return err
}

//go:embed templates/*.tmpl scripts/*.sh
var files embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{"quote": strconv.Quote}).ParseFS(files, "templates/*.tmpl"))

// data is the input to every template.
type data struct {
	OADPNamespace, DPA, LCANamespace, PullSecret, Bucket string
	AccessKey, SecretKey, S3URL, Auth                    string
}

func render(name string, d data) (string, error) {
	d.OADPNamespace, d.DPA, d.LCANamespace, d.PullSecret, d.Bucket = oadpNamespace, dpaName, seed.LCANamespace, PullSecret, manifests.DefaultS3Bucket
	var b bytes.Buffer
	if err := tmpl.ExecuteTemplate(&b, name, d); err != nil {
		return "", fmt.Errorf("rendering %s: %w", name, err)
	}
	return b.String(), nil
}
