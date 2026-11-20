package guestctl

import (
	"bytes"
	"context"
	_ "embed" // the operator template
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/sebrandon1/vsno/lib/precheck"
)

// Operator is an OLM operator vsno installs on a vSNO from the vSNO's own
// catalog, so its version matches the vSNO's.
type Operator struct {
	Name    string // shown in progress and errors, e.g. "LCA"
	Package string // the OLM package, which also names its Subscription and OperatorGroup
	// CSVPrefix starts the names of its ClusterServiceVersions, which needn't
	// be the package's: OADP's package is redhat-oadp-operator, its CSVs
	// oadp-operator.v1.5.8.
	CSVPrefix string
	Namespace string
	// Management marks the namespace for management workloads, which LCA's
	// pods need on a single-node cluster with workload partitioning.
	Management bool
}

// PackageManifest is the part of an operator's catalog entry vsno reads.
type PackageManifest struct {
	Status struct {
		CatalogSource          string `json:"catalogSource"`
		CatalogSourceNamespace string `json:"catalogSourceNamespace"`
		DefaultChannel         string `json:"defaultChannel"`
	} `json:"status"`
}

// CSV returns the name of the operator's ClusterServiceVersion and whether it
// has Succeeded; the name is "" when it isn't installed.
func (op Operator) CSV(ctx context.Context, a Access) (string, bool, error) {
	var csvs ObjectList
	if _, err := Get(ctx, a, &csvs, "get", "clusterserviceversions", "-n", op.Namespace, "--ignore-not-found"); err != nil {
		return "", false, err
	}
	for _, c := range csvs.Items {
		if strings.HasPrefix(c.Metadata.Name, op.CSVPrefix) {
			return c.Metadata.Name, c.Status.Phase == "Succeeded", nil
		}
	}
	return "", false, nil
}

// Catalog looks the operator up in the vSNO's catalog, where Ensure takes its
// channel and source from.
func (op Operator) Catalog(ctx context.Context, a Access) (PackageManifest, bool, error) {
	var pm PackageManifest
	found, err := Get(ctx, a, &pm, "get", "packagemanifests", op.Package, "-n", "openshift-marketplace", "--ignore-not-found")
	return pm, found, err
}

// Ensure installs the operator unless it's there, and waits up to limit for
// its CSV to succeed. It returns the CSV's name.
func (op Operator) Ensure(ctx context.Context, a Access, c Clock, limit time.Duration, ps precheck.ProgressStep) (string, error) {
	name, ok, err := op.CSV(ctx, a)
	if err != nil || ok {
		return name, err
	}
	if name == "" {
		pm, found, err := op.Catalog(ctx, a)
		if err != nil {
			return "", err
		}
		if !found || pm.Status.DefaultChannel == "" {
			return "", fmt.Errorf("the vSNO's operator catalog has no %s package (expected for a nightly before its release)", op.Package)
		}
		y, err := op.Manifests(pm.Status.DefaultChannel, pm.Status.CatalogSource, pm.Status.CatalogSourceNamespace)
		if err != nil {
			return "", err
		}
		if err := Apply(ctx, a, y); err != nil {
			return "", err
		}
		ps.Update(fmt.Sprintf("subscribed to %s/%s", pm.Status.CatalogSource, pm.Status.DefaultChannel))
	}
	err = Poll(ctx, c, limit, func() (bool, error) {
		name, ok, err = op.CSV(ctx, a)
		if name != "" {
			ps.Update("installing " + name)
		}
		return ok, err
	})
	if errors.Is(err, ErrTimeout) {
		return name, fmt.Errorf("%s wasn't installed after %s (`vsno oc <name> -- get csv,installplan -n %s`)", op.Name, limit, op.Namespace)
	}
	return name, err
}

//go:embed templates/operator.yaml.tmpl
var operatorTemplate string

var operatorTmpl = template.Must(template.New("operator").Funcs(template.FuncMap{"quote": strconv.Quote}).Parse(operatorTemplate))

// Manifests are the operator's Namespace, OperatorGroup and Subscription to
// channel from source in sourceNamespace.
func (op Operator) Manifests(channel, source, sourceNamespace string) (string, error) {
	var b bytes.Buffer
	err := operatorTmpl.Execute(&b, struct {
		Operator
		Channel, Source, SourceNamespace string
	}{op, channel, source, sourceNamespace})
	if err != nil {
		return "", fmt.Errorf("rendering %s's manifests: %w", op.Name, err)
	}
	return b.String(), nil
}
