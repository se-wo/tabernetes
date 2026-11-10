package main

import (
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubernetes/pkg/kubectl/cmd/convert"
)

// addConvert adds "kubectl convert". Upstream it is the plugin
// kubectl-convert, but plugins are processes, and js/wasm has none. The
// plugin is only cmd/kubectl-convert around this package, and the internal
// API types it converts through are in the binary for kube-apiserver anyway.
func addConvert(root *cobra.Command, flags *genericclioptions.ConfigFlags, streams genericiooptions.IOStreams) {
	f := cmdutil.NewFactory(cmdutil.NewMatchVersionFlags(flags))
	root.AddCommand(convert.NewCmdConvert(f, streams))
}
