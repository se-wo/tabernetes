package main

// STUB: replaced by tabnode_images.go (images agent). Delete this file then.

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"mvdan.cc/sh/v3/interp"
)

// lookupImage returns the built-in image for an image reference, or nil.
func lookupImage(ref string) *builtinImage { return nil }

// builtinImageNames lists the short names of the built-in images, for messages.
func builtinImageNames() []string { return nil }

// builtinImageList is node.status.images.
func builtinImageList() []v1.ContainerImage { return nil }

// imageID is the imageID of a container status.
func imageID(ref string, img *builtinImage, wasm bool) string { return "tabernetes://" + ref }

// imagePrograms are the programs of the built-in images, by base name.
var imagePrograms = map[string]func(ctx context.Context, hc interp.HandlerContext, args []string, exec interp.ExecHandlerFunc) error{}
