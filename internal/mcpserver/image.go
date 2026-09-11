package mcpserver

import (
	"context"

	"github.com/meigma/codemode"
	"github.com/meigma/codemode/authz"
)

type imageListIn struct {
	OS       *string `json:"os,omitempty"`
	Desktop  *bool   `json:"desktop,omitempty"`
	Platform *string `json:"platform,omitempty"`
}

type imageListItem struct {
	Name        string `json:"name"`
	OS          string `json:"os"`
	Version     string `json:"version"`
	Kind        string `json:"kind"`
	Desktop     bool   `json:"desktop"`
	Platform    string `json:"platform"`
	Description string `json:"description"`
}

type imageListOut struct {
	Items []imageListItem `json:"items"`
}

type imageAPI struct {
	images imageService
}

func registerImage(builder *codemode.Builder, deps Dependencies) {
	api := imageAPI{images: deps.Image}
	codemode.Register(builder, codemode.Capability[imageListIn, imageListOut]{
		ID:      capabilityImageList,
		Name:    capabilityImageList,
		Summary: "List curated images filtered by OS, desktop, or platform.",
		Handler: api.list,
	})
}

func (api imageAPI) list(
	_ context.Context,
	_ authz.Subject,
	in imageListIn,
) (imageListOut, error) {
	images := api.images.ListImages(deref(in.OS, ""), in.Desktop, deref(in.Platform, ""))
	items := make([]imageListItem, 0, len(images))
	for _, image := range images {
		items = append(items, imageListItemDTO(image))
	}
	return imageListOut{Items: items}, nil
}
