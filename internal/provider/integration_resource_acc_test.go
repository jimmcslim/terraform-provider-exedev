package provider

import (
	"fmt"
	"os"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// Acceptance tests run against a real exe.dev account. They need TF_ACC=1 and
// an EXEDEV_TOKEN whose cmds include "integrations list", "integrations add",
// "integrations edit", "integrations rename", "integrations attach",
// "integrations detach" and "integrations remove". EXEDEV_ACC_ATTACH_TAG sets
// the tag the test integration attaches to (default tag:tf-acc-exedev).

var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"exedev": providerserver.NewProtocol6WithError(New()),
}

func testAccPreCheck(t *testing.T) {
	t.Helper()
	if os.Getenv("EXEDEV_TOKEN") == "" {
		t.Fatal("EXEDEV_TOKEN must be set for acceptance tests")
	}
}

func testAccAttachTag() string {
	if tag := os.Getenv("EXEDEV_ACC_ATTACH_TAG"); tag != "" {
		return tag
	}
	return "tag:tf-acc-exedev"
}

func testAccIntegrationConfig(name, audience, clientID string, attach bool) string {
	attachLine := ""
	if attach {
		attachLine = fmt.Sprintf("attach = [%q]", testAccAttachTag())
	}
	return fmt.Sprintf(`
resource "exedev_integration" "test" {
  name     = %q
  consumer = "tailscale"
  audience = %q
  metadata = {
    client_id = %q
  }
  %s
}
`, name, audience, clientID, attachLine)
}

func TestAccIntegrationResource(t *testing.T) {
	name := "tf-acc-" + acctest.RandStringFromCharSet(8, acctest.CharSetAlphaNum)
	renamed := name + "-r"
	const addr = "exedev_integration.test"

	var subject string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// Create with a placeholder audience, as a relying party's ID isn't known yet.
			{
				Config: testAccIntegrationConfig(name, "api.tailscale.com/pending", "pending", true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "id", name),
					resource.TestCheckResourceAttr(addr, "kind", "wif"),
					resource.TestCheckResourceAttr(addr, "team", "false"),
					resource.TestCheckResourceAttr(addr, "attach.#", "1"),
					resource.TestMatchResourceAttr(addr, "issuer", regexpIssuer),
					resource.TestMatchResourceAttr(addr, "subject", regexpNonEmpty),
					func(s *terraform.State) error {
						subject = s.RootModule().Resources[addr].Primary.Attributes["subject"]
						return nil
					},
				),
			},
			// Fill in the real audience and metadata in place; the subject must not change.
			{
				Config: testAccIntegrationConfig(name, "api.tailscale.com/abc123", "abc123", true),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "audience", "api.tailscale.com/abc123"),
					resource.TestCheckResourceAttr(addr, "metadata.client_id", "abc123"),
					func(s *terraform.State) error {
						if got := s.RootModule().Resources[addr].Primary.Attributes["subject"]; got != subject {
							return fmt.Errorf("subject changed on edit: %q -> %q", subject, got)
						}
						return nil
					},
				),
			},
			// Detach and rename in place.
			{
				Config: testAccIntegrationConfig(renamed, "api.tailscale.com/abc123", "abc123", false),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "id", renamed),
					resource.TestCheckNoResourceAttr(addr, "attach.#"),
				),
			},
			// Import by name.
			{
				ResourceName:      addr,
				ImportState:       true,
				ImportStateId:     renamed,
				ImportStateVerify: true,
			},
		},
	})
}

var (
	regexpIssuer   = regexp.MustCompile(`^https://exe\.dev/issuer/.+`)
	regexpNonEmpty = regexp.MustCompile(`.+`)
)
