package main

import (
	"context"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/mechane-ai/terraform-provider-mechane/internal/provider"
)

var version = "dev"

func main() {
	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/mechane-ai/mechane",
	})
	if err != nil {
		log.Fatal(err)
	}
}
