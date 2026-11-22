package main

import (
	"context"
	"cove/terraform-provider-cove/internal/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"log"
)

func main() {
	if err := providerserver.Serve(context.Background(), provider.New, providerserver.ServeOpts{Address: "registry.terraform.io/bwmp-dev/cove"}); err != nil {
		log.Fatal(err)
	}
}
