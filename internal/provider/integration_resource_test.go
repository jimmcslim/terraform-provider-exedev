package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func strMap(t *testing.T, m map[string]string) types.Map {
	t.Helper()
	v, diags := types.MapValueFrom(context.Background(), types.StringType, m)
	if diags.HasError() {
		t.Fatal(diags)
	}
	return v
}

func strSet(t *testing.T, s ...string) types.Set {
	t.Helper()
	elems := make([]attr.Value, len(s))
	for i, v := range s {
		elems[i] = types.StringValue(v)
	}
	v, diags := types.SetValue(types.StringType, elems)
	if diags.HasError() {
		t.Fatal(diags)
	}
	return v
}

func TestBuildIntegrationAddCommand(t *testing.T) {
	ctx := context.Background()
	plan := integrationResourceModel{
		Name:     types.StringValue("tailscale-wif"),
		Consumer: types.StringValue("tailscale"),
		Audience: types.StringValue("api.tailscale.com/pending"),
		Metadata: strMap(t, map[string]string{"tags": "tag:vm-exe-dev", "client_id": "abc123"}),
		Attach:   strSet(t, "vm:rex", "tag:vm-exe-dev"),
		Team:     types.BoolValue(false),
		Comment:  types.StringNull(),
	}
	got := buildIntegrationAddCommand(ctx, plan)
	want := "integrations add wif --name=tailscale-wif --audience=api.tailscale.com/pending --consumer=tailscale" +
		" --metadata='client_id=abc123' --metadata='tags=tag:vm-exe-dev'" +
		" --attach=tag:vm-exe-dev --attach=vm:rex"
	if got != want {
		t.Errorf("add command\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildIntegrationAddCommandTeamAndComment(t *testing.T) {
	ctx := context.Background()
	plan := integrationResourceModel{
		Name:     types.StringValue("gcpwif"),
		Consumer: types.StringUnknown(),
		Audience: types.StringValue("https://iam.googleapis.com/projects/1/x"),
		Metadata: types.MapNull(types.StringType),
		Attach:   types.SetNull(types.StringType),
		Team:     types.BoolValue(true),
		Comment:  types.StringValue("it's managed"),
	}
	got := buildIntegrationAddCommand(ctx, plan)
	want := "integrations add wif --name=gcpwif --audience=https://iam.googleapis.com/projects/1/x --comment='it'\\''s managed' --team"
	if got != want {
		t.Errorf("add command\n got: %s\nwant: %s", got, want)
	}
}

func TestBuildIntegrationEditCommandSendsWholeMetadataMap(t *testing.T) {
	ctx := context.Background()
	plan := integrationResourceModel{
		Audience: types.StringValue("api.tailscale.com/abc123"),
		Metadata: strMap(t, map[string]string{"client_id": "abc123", "tags": "tag:vm-exe-dev"}),
		Team:     types.BoolValue(false),
		Comment:  types.StringNull(),
	}
	got := buildIntegrationEditCommand(ctx, "tailscale-wif", plan)
	want := "integrations edit tailscale-wif --audience=api.tailscale.com/abc123" +
		" --metadata='client_id=abc123' --metadata='tags=tag:vm-exe-dev' --comment=''"
	if got != want {
		t.Errorf("edit command\n got: %s\nwant: %s", got, want)
	}
}

func TestDiffAttachments(t *testing.T) {
	ctx := context.Background()
	attach, detach := diffAttachments(ctx, strSet(t, "vm:a", "tag:b"), strSet(t, "tag:b", "tag:c"))
	if len(attach) != 1 || attach[0] != "tag:c" {
		t.Errorf("attach = %v, want [tag:c]", attach)
	}
	if len(detach) != 1 || detach[0] != "vm:a" {
		t.Errorf("detach = %v, want [vm:a]", detach)
	}

	attach, detach = diffAttachments(ctx, types.SetNull(types.StringType), strSet(t, "vm:a"))
	if len(attach) != 1 || len(detach) != 0 {
		t.Errorf("from null: attach=%v detach=%v", attach, detach)
	}
}

func TestParseIntegrationImportID(t *testing.T) {
	for id, want := range map[string]struct {
		name string
		team bool
	}{
		"tailscale-wif":      {"tailscale-wif", false},
		"team/tailscale-wif": {"tailscale-wif", true},
	} {
		name, team := parseIntegrationImportID(id)
		if name != want.name || team != want.team {
			t.Errorf("%q: got (%q, %v), want (%q, %v)", id, name, team, want.name, want.team)
		}
	}
}

const listFixture = `[
 {"name":"llm","type":"llm","attachments":["auto:all"],"isTeam":false,"config":{"providers":{}}},
 {"name":"gcpwif","type":"wif","attachments":["vm:example-vm"],"isTeam":false,"comment":"",
  "config":{"issuer_id":"example-team-workload","subject":"sub-ABCDEFGHIJKLMNOPQRSTUVWXYZ","consumer":"gcp",
   "audience":"https://iam.googleapis.com/x","metadata":{"project_id":"p","pool_id":"exe-dev-pool"}}},
 {"name":"gcpwif","type":"wif","attachments":["tag:prod"],"isTeam":true,"config":{"issuer_id":"team-x","subject":"sub-TEAM"}}
]`

func TestParseAndFindIntegration(t *testing.T) {
	list, err := parseIntegrationList(trimBOM(append([]byte{0xEF, 0xBB, 0xBF}, listFixture...)))
	if err != nil {
		t.Fatal(err)
	}
	personal, ok := findIntegration(list, "gcpwif", false)
	if !ok || personal.Config.Subject != "sub-ABCDEFGHIJKLMNOPQRSTUVWXYZ" {
		t.Fatalf("personal gcpwif not found or wrong: %+v", personal)
	}
	team, ok := findIntegration(list, "gcpwif", true)
	if !ok || team.Config.Subject != "sub-TEAM" {
		t.Fatalf("team gcpwif not found or wrong: %+v", team)
	}
	if _, ok := findIntegration(list, "missing", false); ok {
		t.Fatal("found a missing integration")
	}
}

func TestApplyIntegrationResponse(t *testing.T) {
	ctx := context.Background()
	list, _ := parseIntegrationList([]byte(listFixture))
	in, _ := findIntegration(list, "gcpwif", false)

	m := integrationResourceModel{
		Consumer: types.StringUnknown(),
		Audience: types.StringValue("https://iam.googleapis.com/x"),
		Metadata: types.MapNull(types.StringType),
		Comment:  types.StringNull(),
	}
	applyIntegrationResponse(ctx, &m, in)

	if got := m.Issuer.ValueString(); got != "https://exe.dev/issuer/example-team-workload" {
		t.Errorf("issuer = %q", got)
	}
	if got := m.Subject.ValueString(); got != "sub-ABCDEFGHIJKLMNOPQRSTUVWXYZ" {
		t.Errorf("subject = %q", got)
	}
	if got := m.Consumer.ValueString(); got != "gcp" {
		t.Errorf("consumer = %q", got)
	}
	if got := len(m.Metadata.Elements()); got != 2 {
		t.Errorf("metadata has %d entries, want 2", got)
	}
	if !m.Comment.IsNull() {
		t.Errorf("comment = %v, want null", m.Comment)
	}
	if got := len(m.Attach.Elements()); got != 1 {
		t.Errorf("attach has %d entries, want 1", got)
	}
}

func TestApplyIntegrationResponseKeepsUnreportedFields(t *testing.T) {
	ctx := context.Background()
	list, _ := parseIntegrationList([]byte(listFixture))
	in, _ := findIntegration(list, "gcpwif", true) // no consumer, audience or metadata in config

	md := strMap(t, map[string]string{"client_id": "abc"})
	m := integrationResourceModel{
		Consumer: types.StringValue("tailscale"),
		Audience: types.StringValue("api.tailscale.com/abc"),
		Metadata: md,
		Comment:  types.StringNull(),
	}
	applyIntegrationResponse(ctx, &m, in)

	if m.Consumer.ValueString() != "tailscale" || m.Audience.ValueString() != "api.tailscale.com/abc" || !m.Metadata.Equal(md) {
		t.Errorf("unreported fields changed: %+v", m)
	}
}
