package httpapi

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"

	"time"

	"github.com/gin-gonic/gin"

	"flowed/internal/platform/config"
)

// OpenAPI generation.
//
// The specification is produced from the router itself rather than written
// beside it. A hand-maintained document drifts the first week somebody adds an
// endpoint in a hurry, and a client generated from a drifted document fails in
// a way that looks like a server bug. Here the paths, methods, path parameters
// and authentication requirements come from the live route table, the request
// and response schemas come from the Go types the handlers actually bind and
// return, and a test fails the build when the checked-in document no longer
// matches what the router serves.
//
// What is deliberately hand-written is the prose: what an endpoint is for, and
// which failures a caller has to handle. Nothing can derive that from a route
// table, and a specification without it is a list of URLs.

// Spec is a minimal OpenAPI 3.1 document.
//
// Hand-rolled rather than pulled from a generator dependency: the document this
// API needs is paths, schemas and security, and a code generator that also
// understands callbacks, links and discriminators is a large surface to keep
// current for output nobody would read differently.
type Spec struct {
	OpenAPI    string                `json:"openapi" yaml:"openapi"`
	Info       Info                  `json:"info" yaml:"info"`
	Servers    []Server              `json:"servers,omitempty" yaml:"servers,omitempty"`
	Tags       []Tag                 `json:"tags,omitempty" yaml:"tags,omitempty"`
	Paths      map[string]PathItem   `json:"paths" yaml:"paths"`
	Components Components            `json:"components" yaml:"components"`
	Security   []map[string][]string `json:"security,omitempty" yaml:"security,omitempty"`
	Extra      map[string]any        `json:"-" yaml:"-"`
}

// Info describes the API.
type Info struct {
	Title       string `json:"title" yaml:"title"`
	Version     string `json:"version" yaml:"version"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

// Server is a base URL.
type Server struct {
	URL         string `json:"url" yaml:"url"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

// Tag groups operations.
type Tag struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

// PathItem is the operations on one path.
type PathItem map[string]Operation

// Operation is one endpoint.
type Operation struct {
	OperationID string                `json:"operationId" yaml:"operationId"`
	Summary     string                `json:"summary" yaml:"summary"`
	Description string                `json:"description,omitempty" yaml:"description,omitempty"`
	Tags        []string              `json:"tags,omitempty" yaml:"tags,omitempty"`
	Parameters  []Parameter           `json:"parameters,omitempty" yaml:"parameters,omitempty"`
	RequestBody *RequestBody          `json:"requestBody,omitempty" yaml:"requestBody,omitempty"`
	Responses   map[string]Response   `json:"responses" yaml:"responses"`
	Security    []map[string][]string `json:"security,omitempty" yaml:"security,omitempty"`
}

// Parameter is a path, query or header input.
type Parameter struct {
	Name        string `json:"name" yaml:"name"`
	In          string `json:"in" yaml:"in"`
	Required    bool   `json:"required,omitempty" yaml:"required,omitempty"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	Schema      Schema `json:"schema" yaml:"schema"`
}

// RequestBody is what an operation accepts.
type RequestBody struct {
	Required bool                 `json:"required" yaml:"required"`
	Content  map[string]MediaType `json:"content" yaml:"content"`
}

// Response is what an operation returns.
type Response struct {
	Description string               `json:"description" yaml:"description"`
	Content     map[string]MediaType `json:"content,omitempty" yaml:"content,omitempty"`
}

// MediaType carries a schema.
type MediaType struct {
	Schema Schema `json:"schema" yaml:"schema"`
}

// Schema is a JSON Schema fragment.
type Schema struct {
	Ref         string            `json:"$ref,omitempty" yaml:"$ref,omitempty"`
	Type        string            `json:"type,omitempty" yaml:"type,omitempty"`
	Format      string            `json:"format,omitempty" yaml:"format,omitempty"`
	Description string            `json:"description,omitempty" yaml:"description,omitempty"`
	Items       *Schema           `json:"items,omitempty" yaml:"items,omitempty"`
	Properties  map[string]Schema `json:"properties,omitempty" yaml:"properties,omitempty"`
	Required    []string          `json:"required,omitempty" yaml:"required,omitempty"`
	Enum        []string          `json:"enum,omitempty" yaml:"enum,omitempty"`
	Nullable    bool              `json:"nullable,omitempty" yaml:"nullable,omitempty"`
}

// MarshalJSON writes the schema as OpenAPI 3.1 rather than 3.0.
//
// 3.1 is JSON Schema 2020-12, which has no `nullable` keyword: an optional
// value is a union with "null". A nullable $ref has to be wrapped rather than
// widened in place, because $ref admits no sibling that would apply to it.
// Emitting `nullable` into a document whose own version field says 3.1 is what
// made the published contract fail validation against its own specification.
func (s Schema) MarshalJSON() ([]byte, error) {
	// A distinct type: methods do not come with it, so this cannot recurse.
	type wire struct {
		Ref         string            `json:"$ref,omitempty"`
		AnyOf       []Schema          `json:"anyOf,omitempty"`
		Type        any               `json:"type,omitempty"`
		Format      string            `json:"format,omitempty"`
		Description string            `json:"description,omitempty"`
		Items       *Schema           `json:"items,omitempty"`
		Properties  map[string]Schema `json:"properties,omitempty"`
		Required    []string          `json:"required,omitempty"`
		Enum        []string          `json:"enum,omitempty"`
	}

	out := wire{
		Ref:         s.Ref,
		Format:      s.Format,
		Description: s.Description,
		Items:       s.Items,
		Properties:  s.Properties,
		Required:    s.Required,
		Enum:        s.Enum,
	}
	switch {
	case s.Nullable && s.Ref != "":
		out.Ref = ""
		out.AnyOf = []Schema{{Ref: s.Ref}, {Type: "null"}}
	case s.Nullable && s.Type != "":
		out.Type = []string{s.Type, "null"}
	case s.Type != "":
		out.Type = s.Type
	}
	return json.Marshal(out)
}

// Components holds reusable schemas and the security scheme.
type Components struct {
	Schemas         map[string]Schema         `json:"schemas" yaml:"schemas"`
	SecuritySchemes map[string]SecurityScheme `json:"securitySchemes" yaml:"securitySchemes"`
}

// SecurityScheme describes how a caller authenticates.
type SecurityScheme struct {
	Type         string `json:"type" yaml:"type"`
	Scheme       string `json:"scheme,omitempty" yaml:"scheme,omitempty"`
	BearerFormat string `json:"bearerFormat,omitempty" yaml:"bearerFormat,omitempty"`
	Description  string `json:"description,omitempty" yaml:"description,omitempty"`
}

// operationMeta is the prose and the types a route cannot describe about
// itself.
type operationMeta struct {
	Summary     string
	Description string
	// Request is a zero value of the type the handler binds. Reflected over to
	// produce the schema, so a field added to the request struct appears in
	// the specification without anybody editing it.
	Request any
	// Response is a zero value of what the handler returns on success.
	Response any
	// Roles that may call it, for the description. Taken from the route's own
	// middleware would be better, but gin does not expose which middleware a
	// route carries — so these are stated and the authorisation tests are what
	// keep them honest.
	Roles []string
	// Public marks an operation that needs no credential.
	Public bool
	// Idempotent marks a money-moving operation that requires the
	// Idempotency-Key header.
	Idempotent bool
}

// publicPaths need no credential. Listed explicitly rather than pattern
// matched: a prefix rule would silently make anything mounted under it public
// later.
var publicPaths = map[string]bool{
	"GET /health":                       true,
	"GET /ready":                        true,
	"POST /api/v1/auth/login":           true,
	"POST /api/v1/auth/refresh":         true,
	"POST /webhooks/payments/:provider": true,
	"GET /verify/statement/:code":       true,
	// The sign-in form needs the desk list before anybody is signed in: a
	// cashier cannot get a token without naming a desk. It carries a code and
	// an Arabic name, both printed on every receipt the university hands out.
}

// BuildSpec produces the specification from a router.
func BuildSpec(engine *gin.Engine, version string) Spec {
	spec := Spec{
		OpenAPI: "3.1.0",
		Info: Info{
			Title:   "Flowed — university tuition and collection API",
			Version: version,
			Description: strings.TrimSpace(`
Money is whole Iraqi dinars as JSON integers: there is no minor unit and no
decimal anywhere in this API. A fractional amount is rejected rather than
rounded.

Every money-moving POST requires an ` + "`Idempotency-Key`" + ` header. A retry with
the same key replays the original response and sets ` + "`Idempotent-Replay: true`" + `;
the same key with a different body is refused outright, because the alternative
is a receipt for a collection that never happened.

Errors share one shape and the code is stable enough to branch on:

    {"error": {"code": "payment.has_refunds", "message": "…", "details": {…}, "request_id": "…"}}

Authorisation has two independent halves. Roles say what an operator may do;
organisational scope says whose data they may do it to. A request can be
refused by either, and the codes differ (` + "`insufficient_role`" + ` and
` + "`outside_scope`" + `) so a client can say which.`),
		},
		Servers: []Server{{URL: "/", Description: "This deployment"}},
		Paths:   map[string]PathItem{},
		Components: Components{
			Schemas: map[string]Schema{},
			SecuritySchemes: map[string]SecurityScheme{
				"bearerAuth": {
					Type:         "http",
					Scheme:       "bearer",
					BearerFormat: "JWT",
					Description: "An access token from /api/v1/auth/login. Tokens carry the actor's " +
						"roles and organisational scope; revoking the session behind one takes " +
						"effect on the next request.",
				},
			},
		},
		Security: []map[string][]string{{"bearerAuth": {}}},
	}

	seenTags := map[string]bool{}
	for _, route := range engine.Routes() {
		key := route.Method + " " + route.Path
		// The operator interface is served by this binary but is not part of
		// the API contract: it is a static page, and a client generator has no
		// use for "GET /app/{filepath}". Excluded here rather than filtered by
		// whoever reads the document.
		if strings.HasPrefix(route.Path, "/app/") {
			continue
		}
		meta := operationMetadata[key]

		operation := Operation{
			OperationID: operationID(route.Method, route.Path),
			Summary:     meta.Summary,
			Description: describeOperation(meta),
			Tags:        []string{tagFor(route.Path)},
			Parameters:  parametersFor(route.Path, meta),
			Responses:   responsesFor(&spec, meta),
		}
		if publicPaths[key] || meta.Public {
			// An empty security array is how OpenAPI says "no credential
			// required", overriding the document-level default.
			operation.Security = []map[string][]string{}
		}
		if meta.Request != nil {
			operation.RequestBody = &RequestBody{
				Required: true,
				Content: map[string]MediaType{
					"application/json": {Schema: schemaRef(&spec, meta.Request)},
				},
			}
		}

		seenTags[operation.Tags[0]] = true
		// Keyed by the OpenAPI template rather than gin's own syntax: a reader
		// of the contract matches parameters against "{id}", and "/:id" leaves
		// every path parameter looking undeclared.
		templated := templatePath(route.Path)
		item, ok := spec.Paths[templated]
		if !ok {
			item = PathItem{}
		}
		item[strings.ToLower(route.Method)] = operation
		spec.Paths[templated] = item
	}

	tags := make([]string, 0, len(seenTags))
	for tag := range seenTags {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	for _, tag := range tags {
		spec.Tags = append(spec.Tags, Tag{Name: tag, Description: tagDescriptions[tag]})
	}

	registerErrorSchema(&spec)
	return spec
}

// describeOperation assembles the prose a route cannot carry itself.
func describeOperation(meta operationMeta) string {
	parts := make([]string, 0, 3)
	if meta.Description != "" {
		parts = append(parts, meta.Description)
	}
	if len(meta.Roles) > 0 {
		parts = append(parts, "Roles: "+strings.Join(meta.Roles, ", ")+".")
	}
	if meta.Idempotent {
		parts = append(parts,
			"Requires an `Idempotency-Key` header. A retry with the same key replays the "+
				"original response; the same key with a different body is refused.")
	}
	return strings.Join(parts, "\n\n")
}

// operationID derives a stable identifier from the method and path.
func operationID(method, path string) string {
	trimmed := strings.TrimPrefix(path, "/api/v1/")
	trimmed = strings.TrimPrefix(trimmed, "/")

	var b strings.Builder
	b.WriteString(strings.ToLower(method))
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == "" {
			continue
		}
		segment = strings.TrimPrefix(segment, ":")
		parts := strings.FieldsFunc(segment, func(r rune) bool { return r == '-' || r == '_' })
		for _, part := range parts {
			if part == "" {
				continue
			}
			b.WriteString(strings.ToUpper(part[:1]))
			b.WriteString(part[1:])
		}
	}
	return b.String()
}

// templatePath rewrites gin's ":id" as OpenAPI's "{id}". Only the path key is
// translated; the metadata tables and the route lookups stay in gin's syntax,
// which is what the router itself is keyed by.
func templatePath(path string) string {
	if !strings.Contains(path, ":") {
		return path
	}
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if name, ok := strings.CutPrefix(segment, ":"); ok {
			segments[i] = "{" + name + "}"
		}
	}
	return strings.Join(segments, "/")
}

// tagFor groups an operation by the resource its path names.
func tagFor(path string) string {
	trimmed := strings.TrimPrefix(path, "/api/v1/")
	trimmed = strings.TrimPrefix(trimmed, "/")
	if trimmed == "" {
		return "service"
	}
	segment := strings.Split(trimmed, "/")[0]
	if segment == "" || strings.HasPrefix(segment, ":") {
		return "service"
	}
	return segment
}

var tagDescriptions = map[string]string{
	"auth":             "Signing in, renewing, signing out, and the caller's own account.",
	"students":         "Student identity: registration, contact details, court-ordered identity changes, merge.",
	"enrollments":      "The financial unit. Registration, results, status changes and their financial treatment.",
	"accounts":         "Pricing an enrollment and everything that changes what it owes.",
	"payments":         "Collection. Every posting is idempotent and every receipt is immutable.",
	"refunds":          "Returning money, through request, approval and posting.",
	"voids":            "Reversing a collection that should never have existed. Two people, always.",
	"discounts":        "Definitions, grants and the applications frozen onto an account.",
	"sponsors":         "Third parties who pay students' fees, and what they owe.",
	"sponsorships":     "The agreements behind sponsor commitments.",
	"settlements":      "Bank and card statements matched against what was collected.",
	"payment-intents":  "Electronic collection begun at a provider and confirmed by it.",
	"portal":           "What a student may see about themselves.",
	"reports":          "Aggregates, registers and their exports.",
	"users":            "Operator accounts, roles, scope and sessions.",
	"academic-years":   "The year lifecycle, including its two closes.",
	"imports":          "Staged bulk import, with a preview nobody can skip.",
	"bulk":             "Cohort operations, each with a dry run and a plan hash.",
	"cashier-sessions": "Shifts, and the drawer that has to balance at the end of one.",
	"oversight":        "Audit verification and reconciliation.",
	"webhooks":         "Provider callbacks. Authenticated by signature, never by credential.",
	"verify":           "Public verification of a printed statement.",
}

// parametersFor derives path parameters from the route and adds the query
// parameters the convention applies everywhere.
func parametersFor(path string, meta operationMeta) []Parameter {
	var parameters []Parameter

	for _, segment := range strings.Split(path, "/") {
		if !strings.HasPrefix(segment, ":") {
			continue
		}
		name := strings.TrimPrefix(segment, ":")
		parameters = append(parameters, Parameter{
			Name:     name,
			In:       "path",
			Required: true,
			Schema:   Schema{Type: "string", Format: pathFormat(name)},
		})
	}

	// Listing endpoints share one pagination convention, and reports share one
	// export convention. Both are stated here rather than repeated per route.
	if strings.HasSuffix(path, "s") && !strings.Contains(path, ":") {
		parameters = append(parameters,
			Parameter{Name: "limit", In: "query", Description: "Page size, default 50, maximum 500.",
				Schema: Schema{Type: "integer"}},
			Parameter{Name: "offset", In: "query", Description: "Rows to skip.",
				Schema: Schema{Type: "integer"}})
	}
	if strings.Contains(path, "/reports/") || strings.HasSuffix(path, "/receivables") {
		parameters = append(parameters, Parameter{
			Name: "format", In: "query",
			Description: "Omit for JSON. `csv` and `xlsx` download a file; `pdf` returns a " +
				"print-ready page — an Arabic PDF needs font shaping the browser already has.",
			Schema: Schema{Type: "string", Enum: []string{"csv", "xlsx", "pdf"}},
		})
	}
	if meta.Idempotent {
		parameters = append(parameters, Parameter{
			Name: "Idempotency-Key", In: "header", Required: true,
			Description: "Client-generated key. A retry with the same key replays the original " +
				"response rather than collecting twice.",
			Schema: Schema{Type: "string"},
		})
	}
	return parameters
}

// pathFormat marks the identifier parameters as UUIDs.
func pathFormat(name string) string {
	switch name {
	case "id", "line_id", "enrollment_id", "account_id", "provider":
		if name == "provider" {
			return ""
		}
		return "uuid"
	default:
		return ""
	}
}

// responsesFor assembles the response set every operation shares.
func responsesFor(spec *Spec, meta operationMeta) map[string]Response {
	success := Response{Description: "Success."}
	if meta.Response != nil {
		success.Content = map[string]MediaType{
			"application/json": {Schema: schemaRef(spec, meta.Response)},
		}
	}

	errorRef := Schema{Ref: "#/components/schemas/ErrorResponse"}
	return map[string]Response{
		"200": success,
		"400": {Description: "The request is malformed, or a value is not one the domain accepts.",
			Content: map[string]MediaType{"application/json": {Schema: errorRef}}},
		"401": {Description: "No credential, an expired one, or a session that was ended.",
			Content: map[string]MediaType{"application/json": {Schema: errorRef}}},
		"403": {Description: "The actor's roles or organisational scope do not reach this.",
			Content: map[string]MediaType{"application/json": {Schema: errorRef}}},
		"404": {Description: "No such record.",
			Content: map[string]MediaType{"application/json": {Schema: errorRef}}},
		"409": {Description: "A uniqueness or concurrency clash — a duplicate, or a record already in this state.",
			Content: map[string]MediaType{"application/json": {Schema: errorRef}}},
		"422": {Description: "A well-formed request the domain refuses: paying into closed books, " +
			"voiding a payment that carries a refund.",
			Content: map[string]MediaType{"application/json": {Schema: errorRef}}},
		"429": {Description: "Rate limited, or an account locked after repeated failed sign-ins.",
			Content: map[string]MediaType{"application/json": {Schema: errorRef}}},
		"500": {Description: "An internal failure. The response carries a request id and nothing else.",
			Content: map[string]MediaType{"application/json": {Schema: errorRef}}},
	}
}

func registerErrorSchema(spec *Spec) {
	spec.Components.Schemas["ErrorResponse"] = Schema{
		Type:        "object",
		Description: "The one error shape every endpoint uses.",
		Required:    []string{"error"},
		Properties: map[string]Schema{
			"error": {
				Type:     "object",
				Required: []string{"code", "message"},
				Properties: map[string]Schema{
					"code": {Type: "string", Description: "Stable machine code. Branch on this, never on the message."},
					"message": {Type: "string",
						Description: "Human-readable. Refusals say what to do instead."},
					"details":    {Type: "object", Description: "Structured context, including a `remedy` where one exists."},
					"request_id": {Type: "string", Description: "Correlates with the server log."},
				},
			},
		},
	}
}

// schemaRef registers a Go type's schema and returns a reference to it.
//
// Reflection over the DTO the handler actually binds or returns, so a field
// added to the struct appears in the specification without anyone editing the
// specification. That is the whole point: the document cannot drift from the
// code because it is derived from it.
func schemaRef(spec *Spec, value any) Schema {
	t := reflect.TypeOf(value)
	if t == nil {
		return Schema{Type: "object"}
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() == reflect.Slice {
		item := schemaRef(spec, reflect.New(t.Elem()).Elem().Interface())
		return Schema{Type: "array", Items: &item}
	}
	if t.Kind() != reflect.Struct {
		return schemaForType(spec, t)
	}

	name := t.Name()
	if name == "" {
		return Schema{Type: "object"}
	}
	if _, exists := spec.Components.Schemas[name]; !exists {
		// Registered before recursing, so a self-referential type terminates.
		spec.Components.Schemas[name] = Schema{Type: "object"}
		spec.Components.Schemas[name] = structSchema(spec, t)
	}
	return Schema{Ref: "#/components/schemas/" + name}
}

// structSchema reflects one struct into a JSON Schema object.
func structSchema(spec *Spec, t reflect.Type) Schema {
	schema := Schema{Type: "object", Properties: map[string]Schema{}}

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		if field.Anonymous {
			// Embedded structs contribute their fields directly, which is what
			// the JSON encoder does too.
			embedded := structSchema(spec, field.Type)
			for name, property := range embedded.Properties {
				schema.Properties[name] = property
			}
			schema.Required = append(schema.Required, embedded.Required...)
			continue
		}

		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name := field.Name
		if parts := strings.Split(tag, ","); parts[0] != "" {
			name = parts[0]
		}

		property := schemaForType(spec, field.Type)
		if binding := field.Tag.Get("binding"); binding != "" {
			property.Description = bindingDescription(binding)
			if strings.Contains(binding, "required") {
				schema.Required = append(schema.Required, name)
			}
			if values := enumValues(binding); len(values) > 0 {
				property.Enum = values
			}
		}
		schema.Properties[name] = property
	}
	sort.Strings(schema.Required)
	return schema
}

// schemaForType maps a Go type onto a JSON Schema type.
func schemaForType(spec *Spec, t reflect.Type) Schema {
	nullable := false
	for t.Kind() == reflect.Pointer {
		nullable = true
		t = t.Elem()
	}

	schema := Schema{Nullable: nullable}
	switch t.Kind() {
	case reflect.String:
		schema.Type = "string"
	case reflect.Bool:
		schema.Type = "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		schema.Type = "integer"
		// money.Amount is an int64 of whole dinars. Saying so in the schema is
		// what stops a client generator producing a float and losing a dinar.
		if t.Name() == "Amount" {
			schema.Format = "int64"
			schema.Description = "Whole Iraqi dinars. No minor unit; fractions are refused."
		}
	case reflect.Float32, reflect.Float64:
		schema.Type = "number"
	case reflect.Slice, reflect.Array:
		item := schemaForType(spec, t.Elem())
		schema.Type = "array"
		schema.Items = &item
	case reflect.Map:
		schema.Type = "object"
	case reflect.Struct:
		if t.PkgPath() == "time" && t.Name() == "Time" {
			schema.Type = "string"
			schema.Format = "date-time"
			return schema
		}
		ref := schemaRef(spec, reflect.New(t).Elem().Interface())
		ref.Nullable = nullable
		return ref
	default:
		schema.Type = "object"
	}
	return schema
}

// bindingDescription turns gin's binding tag into prose a reader can use.
func bindingDescription(binding string) string {
	var notes []string
	for _, rule := range strings.Split(binding, ",") {
		switch {
		case rule == "required":
			notes = append(notes, "required")
		case rule == "uuid":
			notes = append(notes, "UUID")
		case rule == "email":
			notes = append(notes, "e-mail address")
		case strings.HasPrefix(rule, "oneof="):
			notes = append(notes, "one of "+strings.ReplaceAll(strings.TrimPrefix(rule, "oneof="), " ", ", "))
		case strings.HasPrefix(rule, "min="):
			notes = append(notes, "minimum "+strings.TrimPrefix(rule, "min="))
		case strings.HasPrefix(rule, "max="):
			notes = append(notes, "maximum "+strings.TrimPrefix(rule, "max="))
		}
	}
	if len(notes) == 0 {
		return ""
	}
	return strings.ToUpper(notes[0][:1]) + notes[0][1:] + joinRest(notes[1:])
}

func joinRest(notes []string) string {
	if len(notes) == 0 {
		return "."
	}
	return "; " + strings.Join(notes, "; ") + "."
}

// enumValues pulls the permitted values out of a oneof binding.
func enumValues(binding string) []string {
	for _, rule := range strings.Split(binding, ",") {
		if strings.HasPrefix(rule, "oneof=") {
			return strings.Fields(strings.TrimPrefix(rule, "oneof="))
		}
	}
	return nil
}

var _ = fmt.Sprintf

// NewSpecRouter builds an engine carrying every route and no behaviour.
//
// It calls the same NewRouter the server does, with typed-nil handler groups.
// Taking a method value off a nil pointer is legal in Go as long as nobody
// calls it, and nobody does: BuildSpec reads the route table. The point is that
// there is exactly one place routes are declared — a second list maintained for
// documentation would drift from the first, which is the failure this whole
// generator exists to prevent.
func NewSpecRouter() *gin.Engine {
	return NewRouter(RouterDeps{
		Config:      specConfig(),
		Handlers:    (*Handlers)(nil),
		Auth:        (*AuthHandlers)(nil),
		Reports:     (*ReportHandlers)(nil),
		ConfigAdmin: (*ConfigHandlers)(nil),
		Bulk:        (*BulkHandlers)(nil),
		Receipts:    (*ReceiptHandlers)(nil),
		UserAdmin:   (*UserHandlers)(nil),
		Lifecycle:   (*LifecycleHandlers)(nil),
		MasterData:  (*MasterDataHandlers)(nil),
		// Mounted with no service behind it: the archive routes exist whether
		// or not a destination is configured, and the specification should say
		// so for the same reason the server does.
		AuditArchive:   NewAuditArchiveHandlers(nil),
		Reconciliation: NewReconciliationHandlers(nil),
		Version:        "spec",
	})
}

// specConfig is the minimum configuration NewRouter reads while wiring.
//
// The middleware it builds is never executed here; what matters is that the
// same wiring runs, so a route mounted behind a configuration switch appears in
// the specification exactly when it appears in the server.
func specConfig() *config.Config {
	return &config.Config{
		App:  config.App{Name: "flowed", Environment: "development"},
		HTTP: config.HTTP{Host: "127.0.0.1", Port: 8080, WriteTimeout: 30 * time.Second},
		Auth: config.Auth{StrictSessionCheck: false},
	}
}

var _ = slog.Default
