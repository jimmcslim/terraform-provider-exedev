package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/benjamin-lykins/terraform-provider-exedev/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

var (
	_ resource.Resource                = &integrationResource{}
	_ resource.ResourceWithImportState = &integrationResource{}
	_ resource.ResourceWithModifyPlan  = &integrationResource{}
)

// integrationKindWIF is the exe.dev integration type for Identity Federation.
const integrationKindWIF = "wif"

// issuerBaseURL prefixes an integration's issuer_id to form its OIDC issuer.
const issuerBaseURL = "https://exe.dev/issuer/"

// integrationResourceModel maps the schema attributes for exedev_integration.
type integrationResourceModel struct {
	ID       types.String `tfsdk:"id"`
	Name     types.String `tfsdk:"name"`
	Kind     types.String `tfsdk:"kind"`
	Consumer types.String `tfsdk:"consumer"`
	Audience types.String `tfsdk:"audience"`
	Metadata types.Map    `tfsdk:"metadata"`
	Attach   types.Set    `tfsdk:"attach"`
	Team     types.Bool   `tfsdk:"team"`
	Comment  types.String `tfsdk:"comment"`
	IssuerID types.String `tfsdk:"issuer_id"`
	Issuer   types.String `tfsdk:"issuer"`
	Subject  types.String `tfsdk:"subject"`
}

// integrationAPIResponse is one entry of `integrations list --json`.
type integrationAPIResponse struct {
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Attachments []string          `json:"attachments"`
	IsTeam      bool              `json:"isTeam"`
	Comment     string            `json:"comment"`
	Config      integrationConfig `json:"config"`
}

// integrationConfig holds the wif-specific fields of an integration.
type integrationConfig struct {
	IssuerID string            `json:"issuer_id"`
	Subject  string            `json:"subject"`
	Consumer string            `json:"consumer_type"`
	Audience string            `json:"audience"`
	Metadata map[string]string `json:"metadata"`
}

type integrationResource struct {
	client *client.Client
}

// NewIntegrationResource returns a new integrationResource factory function.
func NewIntegrationResource() resource.Resource {
	return &integrationResource{}
}

func (r *integrationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_integration"
}

func (r *integrationResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an [exe.dev](https://exe.dev) Identity Federation (`wif`) integration. " +
			"An attached VM can mint short-lived OIDC tokens from `https://<name>.int.exe.xyz/token`; " +
			"the computed `issuer` and `subject` are what the relying party (Tailscale, AWS, GCP, …) must trust.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The integration name.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Integration name. It is also the VM-side hostname, `<name>.int.exe.xyz`. Renamed in place.",
			},
			"kind": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Integration type. Always `wif`.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"consumer": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "The relying party the tokens are for, such as `tailscale`, `aws`, `gcp`, `anthropic` or `openai`. Changing this forces a new integration.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"audience": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The `aud` claim of minted tokens. Updated in place, so a placeholder can be set first and replaced once the relying party has issued its own value.",
			},
			"metadata": schema.MapAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Values the VM reads back from `https://<name>.int.exe.xyz/metadata`, such as a Tailscale `client_id`. `integrations edit` replaces the whole map, so the provider always sends every entry.",
			},
			"attach": schema.SetAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Where the integration is mounted: `vm:<name>`, `tag:<name>` or `auto:all`. Team integrations only accept `tag:<name>`. Updated in place with `integrations attach`/`detach`.",
			},
			"team": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
				MarkdownDescription: "Create a team integration instead of a personal one. Changing this forces a new integration.",
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.RequiresReplace(),
				},
			},
			"comment": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Free-form comment stored with the integration.",
			},
			"issuer_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The user- or team-scoped issuer ID.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"issuer": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The OIDC issuer URL, `https://exe.dev/issuer/<issuer_id>`.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"subject": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The `sub` claim of minted tokens, generated by exe.dev when the integration is created.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

// ModifyPlan marks id unknown when the name changes, since id is the name.
func (r *integrationResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var planName, stateName types.String
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("name"), &planName)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("name"), &stateName)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !planName.Equal(stateName) {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("id"), types.StringUnknown())...)
	}
}

func (r *integrationResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected provider data type",
			fmt.Sprintf("Expected *client.Client, got %T", req.ProviderData),
		)
		return
	}
	r.client = c
}

func (r *integrationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan integrationResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	cmd := buildIntegrationAddCommand(ctx, plan)
	tflog.Debug(ctx, "Adding integration", map[string]any{"cmd": cmd})
	if _, err := r.client.Exec(cmd); err != nil {
		resp.Diagnostics.AddError("Adding integration", err.Error())
		return
	}

	name := plan.Name.ValueString()
	in, found, err := r.readIntegration(ctx, name, plan.Team.ValueBool())
	if err != nil {
		resp.Diagnostics.AddError("Reading integration after add", err.Error())
		return
	}
	if !found {
		resp.Diagnostics.AddError("Reading integration after add", fmt.Sprintf("integration %q not found after add", name))
		return
	}

	applyIntegrationResponse(ctx, &plan, in)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *integrationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state integrationResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	in, found, err := r.readIntegration(ctx, state.ID.ValueString(), state.Team.ValueBool())
	if err != nil {
		resp.Diagnostics.AddError("Reading integration", err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	if in.Type != integrationKindWIF {
		resp.Diagnostics.AddError("Unsupported integration type",
			fmt.Sprintf("integration %q has type %q; exedev_integration only manages %q integrations", in.Name, in.Type, integrationKindWIF))
		return
	}

	applyIntegrationResponse(ctx, &state, in)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *integrationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state integrationResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	team := plan.Team.ValueBool()
	name := state.ID.ValueString()

	if newName := plan.Name.ValueString(); newName != name {
		cmd := fmt.Sprintf("integrations rename %s %s%s", client.ShellQuote(name), client.ShellQuote(newName), teamFlag(team))
		tflog.Debug(ctx, "Renaming integration", map[string]any{"cmd": cmd})
		if _, err := r.client.Exec(cmd); err != nil {
			resp.Diagnostics.AddError("Renaming integration", err.Error())
			return
		}
		name = newName
	}

	if !plan.Audience.Equal(state.Audience) || !plan.Metadata.Equal(state.Metadata) || !plan.Comment.Equal(state.Comment) {
		cmd := buildIntegrationEditCommand(ctx, name, plan)
		tflog.Debug(ctx, "Editing integration", map[string]any{"cmd": cmd})
		if _, err := r.client.Exec(cmd); err != nil {
			resp.Diagnostics.AddError("Editing integration", err.Error())
			return
		}
	}

	toAttach, toDetach := diffAttachments(ctx, state.Attach, plan.Attach)
	for _, spec := range toDetach {
		cmd := fmt.Sprintf("integrations detach %s %s%s", client.ShellQuote(name), client.ShellQuote(spec), teamFlag(team))
		tflog.Debug(ctx, "Detaching integration", map[string]any{"cmd": cmd})
		if _, err := r.client.Exec(cmd); err != nil {
			resp.Diagnostics.AddError("Detaching integration", err.Error())
			return
		}
	}
	for _, spec := range toAttach {
		cmd := fmt.Sprintf("integrations attach %s %s%s", client.ShellQuote(name), client.ShellQuote(spec), teamFlag(team))
		tflog.Debug(ctx, "Attaching integration", map[string]any{"cmd": cmd})
		if _, err := r.client.Exec(cmd); err != nil {
			resp.Diagnostics.AddError("Attaching integration", err.Error())
			return
		}
	}

	in, found, err := r.readIntegration(ctx, name, team)
	if err != nil {
		resp.Diagnostics.AddError("Reading integration after update", err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}

	applyIntegrationResponse(ctx, &plan, in)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *integrationResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state integrationResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	cmd := fmt.Sprintf("integrations remove %s%s", client.ShellQuote(state.ID.ValueString()), teamFlag(state.Team.ValueBool()))
	tflog.Debug(ctx, "Removing integration", map[string]any{"cmd": cmd})
	if _, err := r.client.Exec(cmd); err != nil {
		var execErr *client.ExecError
		if errors.As(err, &execErr) && (execErr.StatusCode == 404 || execErr.StatusCode == 422) {
			// Treat as already gone only if a fresh list agrees.
			if _, found, readErr := r.readIntegration(ctx, state.ID.ValueString(), state.Team.ValueBool()); readErr == nil && !found {
				return
			}
		}
		resp.Diagnostics.AddError("Removing integration", err.Error())
	}
}

// ImportState imports by name. Prefix the name with `team/` to import a team integration.
func (r *integrationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	name, team := parseIntegrationImportID(req.ID)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("team"), team)...)
}

func parseIntegrationImportID(id string) (name string, team bool) {
	if rest, ok := strings.CutPrefix(id, "team/"); ok {
		return rest, true
	}
	return id, false
}

// readIntegration finds an integration by name and scope in `integrations list --json`.
func (r *integrationResource) readIntegration(ctx context.Context, name string, team bool) (integrationAPIResponse, bool, error) {
	cmd := "integrations list --json"
	tflog.Debug(ctx, "Listing integrations", map[string]any{"cmd": cmd})
	body, err := r.client.Exec(cmd)
	if err != nil {
		return integrationAPIResponse{}, false, err
	}
	list, err := parseIntegrationList(trimBOM(body))
	if err != nil {
		return integrationAPIResponse{}, false, err
	}
	in, found := findIntegration(list, name, team)
	return in, found, nil
}

func parseIntegrationList(body []byte) ([]integrationAPIResponse, error) {
	var list []integrationAPIResponse
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parsing integrations list response: %w", err)
	}
	return list, nil
}

func findIntegration(list []integrationAPIResponse, name string, team bool) (integrationAPIResponse, bool) {
	for _, in := range list {
		if in.Name == name && in.IsTeam == team {
			return in, true
		}
	}
	return integrationAPIResponse{}, false
}

// buildIntegrationAddCommand constructs `integrations add wif …` from the plan.
func buildIntegrationAddCommand(ctx context.Context, plan integrationResourceModel) string {
	var sb strings.Builder
	sb.WriteString("integrations add " + integrationKindWIF)
	sb.WriteString(" --name=" + client.ShellQuote(plan.Name.ValueString()))
	sb.WriteString(" --audience=" + client.ShellQuote(plan.Audience.ValueString()))
	if s := knownString(plan.Consumer); s != "" {
		sb.WriteString(" --consumer=" + client.ShellQuote(s))
	}
	writeMetadataFlags(ctx, &sb, plan.Metadata)
	for _, spec := range setStrings(ctx, plan.Attach) {
		sb.WriteString(" --attach=" + client.ShellQuote(spec))
	}
	if s := knownString(plan.Comment); s != "" {
		sb.WriteString(" --comment=" + client.ShellQuote(s))
	}
	sb.WriteString(teamFlag(plan.Team.ValueBool()))
	return sb.String()
}

// buildIntegrationEditCommand constructs `integrations edit …`. It always sends
// the audience, the comment and every metadata entry, because edit replaces
// the whole metadata map.
func buildIntegrationEditCommand(ctx context.Context, name string, plan integrationResourceModel) string {
	var sb strings.Builder
	sb.WriteString("integrations edit " + client.ShellQuote(name))
	sb.WriteString(" --audience=" + client.ShellQuote(plan.Audience.ValueString()))
	writeMetadataFlags(ctx, &sb, plan.Metadata)
	sb.WriteString(" --comment=" + client.ShellQuote(knownString(plan.Comment)))
	sb.WriteString(teamFlag(plan.Team.ValueBool()))
	return sb.String()
}

// writeMetadataFlags appends one --metadata=k=v flag per entry, sorted by key.
func writeMetadataFlags(ctx context.Context, sb *strings.Builder, m types.Map) {
	if m.IsNull() || m.IsUnknown() {
		return
	}
	values := make(map[string]string, len(m.Elements()))
	m.ElementsAs(ctx, &values, false)
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sb.WriteString(" --metadata=" + client.ShellQuote(k+"="+values[k]))
	}
}

// diffAttachments returns the specs to attach and detach to go from state to plan.
func diffAttachments(ctx context.Context, state, plan types.Set) (toAttach, toDetach []string) {
	have := make(map[string]bool)
	for _, s := range setStrings(ctx, state) {
		have[s] = true
	}
	want := make(map[string]bool)
	for _, s := range setStrings(ctx, plan) {
		want[s] = true
		if !have[s] {
			toAttach = append(toAttach, s)
		}
	}
	for _, s := range setStrings(ctx, state) {
		if !want[s] {
			toDetach = append(toDetach, s)
		}
	}
	return toAttach, toDetach
}

func setStrings(ctx context.Context, s types.Set) []string {
	if s.IsNull() || s.IsUnknown() {
		return nil
	}
	out := make([]string, 0, len(s.Elements()))
	s.ElementsAs(ctx, &out, false)
	sort.Strings(out)
	return out
}

func knownString(s types.String) string {
	if s.IsNull() || s.IsUnknown() {
		return ""
	}
	return s.ValueString()
}

func teamFlag(team bool) string {
	if team {
		return " --team"
	}
	return ""
}

// applyIntegrationResponse copies the API's view of an integration into the model.
func applyIntegrationResponse(ctx context.Context, m *integrationResourceModel, in integrationAPIResponse) {
	m.ID = types.StringValue(in.Name)
	m.Name = types.StringValue(in.Name)
	m.Kind = types.StringValue(in.Type)
	m.Team = types.BoolValue(in.IsTeam)

	m.IssuerID = types.StringValue(in.Config.IssuerID)
	if in.Config.IssuerID != "" {
		m.Issuer = types.StringValue(issuerBaseURL + in.Config.IssuerID)
	} else {
		m.Issuer = types.StringValue("")
	}
	m.Subject = types.StringValue(in.Config.Subject)

	// A field the API leaves out keeps its prior value, so a response without
	// it can't cause drift. An empty value the API does report is honoured.
	if in.Config.Consumer != "" || m.Consumer.IsNull() || m.Consumer.IsUnknown() {
		m.Consumer = types.StringValue(in.Config.Consumer)
	}
	if in.Config.Audience != "" || m.Audience.IsNull() || m.Audience.IsUnknown() {
		m.Audience = types.StringValue(in.Config.Audience)
	}
	switch {
	case in.Config.Metadata == nil:
		// Not reported: keep state.
	case len(in.Config.Metadata) == 0:
		if !m.Metadata.IsNull() && len(m.Metadata.Elements()) > 0 {
			m.Metadata = types.MapNull(types.StringType)
		}
	default:
		md, _ := types.MapValueFrom(ctx, types.StringType, in.Config.Metadata)
		m.Metadata = md
	}

	if len(in.Attachments) > 0 {
		att, _ := types.SetValueFrom(ctx, types.StringType, in.Attachments)
		m.Attach = att
	} else {
		m.Attach = types.SetNull(types.StringType)
	}

	if in.Comment != "" {
		m.Comment = types.StringValue(in.Comment)
	} else if !m.Comment.IsNull() {
		m.Comment = types.StringNull()
	}
}
