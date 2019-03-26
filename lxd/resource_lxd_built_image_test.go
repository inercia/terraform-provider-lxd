package lxd

import (
	"testing"

	"github.com/hashicorp/terraform/helper/resource"
	"github.com/lxc/lxd/shared/api"
)

func TestAccBuiltImage_basic(t *testing.T) {
	var img api.Image

	testTfFile := `
	
	        data "template_file" "distrobuilder" {
	          template = <<-EOT
                image:
                  distribution: ubuntu
                  release: cosmic
                  variant: default
                  description: Ubuntu
                  expiry: 30d
                  architecture: amd64
                
                source:
                  downloader: ubuntu-http
                  url: http://cdimage.ubuntu.com/ubuntu-base
                  keys:
                    - 0x46181433FBB75451
	          EOT
            }
	
            resource "lxd_built_image" "img1" {
              template = "${data.template_file.distrobuilder.rendered}"
            }`

	resource.Test(t, resource.TestCase{
		PreCheck:  func() { testAccPreCheck(t) },
		Providers: testAccProviders,
		Steps: []resource.TestStep{
			resource.TestStep{
				Config: testTfFile,
				Check: resource.ComposeTestCheckFunc(
					testAccCachedImageExists(t, "lxd_cached_image.img1", &img),
					resourceAccCachedImageCheckAttributes("lxd_cached_image.img1", &img),
				),
			},
		},
	})
}
