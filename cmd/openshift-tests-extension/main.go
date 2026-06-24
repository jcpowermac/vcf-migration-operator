package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/openshift-eng/openshift-tests-extension/pkg/cmd"
	e "github.com/openshift-eng/openshift-tests-extension/pkg/extension"
	et "github.com/openshift-eng/openshift-tests-extension/pkg/extension/extensiontests"
	g "github.com/openshift-eng/openshift-tests-extension/pkg/ginkgo"

	_ "github.com/openshift/vcf-migration-operator/test/e2e/vsphere"
)

func main() {
	registry := e.NewRegistry()

	ext := e.NewExtension("openshift", "payload", "vcf-migration-operator")

	ext.AddSuite(e.Suite{
		Name:    "vcf-migration/vsphere",
		Parents: []string{"openshift/conformance/parallel"},
	})

	specs, err := g.BuildExtensionTestSpecsFromOpenShiftGinkgoSuite()
	if err != nil {
		panic(fmt.Sprintf("building extension test specs from ginkgo: %s", err))
	}

	specs.Select(et.NameContains("[platform:vsphere]")).
		Include(et.PlatformEquals("vsphere"))

	ext.AddSpecs(specs)
	registry.Register(ext)

	root := &cobra.Command{
		Long: "VCF Migration Operator E2E Tests",
	}
	root.AddCommand(cmd.DefaultExtensionCommands(registry)...)

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
