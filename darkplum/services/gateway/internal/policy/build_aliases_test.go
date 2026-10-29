package policy

import "testing"

// TestBuildAliases_StaticAliasUnchanged asserts static aliases keep working
// unchanged: kind static, target populated, no routed fields.
func TestBuildAliases_StaticAliasUnchanged(t *testing.T) {
	aliases := buildAliases([]rawAliasRow{
		{Alias: "prod-default", Kind: AliasKindStatic, Target: strPtr("llama-3-8b")},
	}, nil)

	def, ok := aliases["prod-default"]
	if !ok {
		t.Fatal("expected prod-default to be present")
	}
	if def.Kind != AliasKindStatic || def.Target != "llama-3-8b" {
		t.Errorf("got %+v", def)
	}
	if target, ok := (Snapshot{Aliases: aliases}).StaticTarget("prod-default"); !ok || target != "llama-3-8b" {
		t.Errorf("StaticTarget = %q, %v; want llama-3-8b, true", target, ok)
	}
}

// TestBuildAliases_StaticAliasWithNilTargetIsDropped covers the defensive
// branch for a row that violates alias_kind_shape (should be unreachable via
// the DB CHECK constraint, but the gateway must not trust that forever).
func TestBuildAliases_StaticAliasWithNilTargetIsDropped(t *testing.T) {
	aliases := buildAliases([]rawAliasRow{
		{Alias: "broken", Kind: AliasKindStatic, Target: nil},
	}, nil)
	if _, ok := aliases["broken"]; ok {
		t.Error("expected a static alias with a nil target to be dropped")
	}
}

// TestBuildAliases_RoutedAliasAssembled is the core routed-alias load path:
// routes are attached in position order and keywords are case-folded.
func TestBuildAliases_RoutedAliasAssembled(t *testing.T) {
	aliasRows := []rawAliasRow{
		{
			Alias:          "smart-chat",
			Kind:           AliasKindRouted,
			DefaultRoute:   strPtr("general"),
			ClassifierKind: strPtr(ClassifierKindRules),
		},
	}
	routeRows := []rawRouteRow{
		{Alias: "smart-chat", Name: "code", Description: "code questions", Target: "qwen-coder", Keywords: []string{"BUILTIN:CODE", "Debug"}},
		{Alias: "smart-chat", Name: "general", Description: "everything else", Target: "llama-3-8b", Keywords: nil},
	}

	aliases := buildAliases(aliasRows, routeRows)

	def, ok := aliases["smart-chat"]
	if !ok {
		t.Fatal("expected smart-chat to be present")
	}
	if def.Kind != AliasKindRouted || def.DefaultRoute != "general" || def.ClassifierKind != ClassifierKindRules {
		t.Errorf("got %+v", def)
	}
	if len(def.Routes) != 2 || def.Routes[0].Name != "code" || def.Routes[1].Name != "general" {
		t.Fatalf("expected routes in position order, got %+v", def.Routes)
	}
	if def.Routes[0].Keywords[0] != "builtin:code" || def.Routes[0].Keywords[1] != "debug" {
		t.Errorf("expected keywords case-folded on load, got %+v", def.Routes[0].Keywords)
	}
	if _, ok := def.Route("general"); !ok {
		t.Error("expected Route(\"general\") to resolve")
	}
}

// TestBuildAliases_RoutedAliasWithZeroRoutesDropped is the core item-1
// consistency guard: zero routes must drop the alias, not panic or resolve to
// an empty target.
func TestBuildAliases_RoutedAliasWithZeroRoutesDropped(t *testing.T) {
	aliases := buildAliases([]rawAliasRow{
		{Alias: "ghost-router", Kind: AliasKindRouted, DefaultRoute: strPtr("general")},
	}, nil)
	if _, ok := aliases["ghost-router"]; ok {
		t.Error("expected a routed alias with zero routes to be dropped")
	}
}

// TestBuildAliases_RoutedAliasWithMissingDefaultRouteDropped covers the other
// half of the consistency guard: default_route must name one of the loaded
// routes.
func TestBuildAliases_RoutedAliasWithMissingDefaultRouteDropped(t *testing.T) {
	aliases := buildAliases([]rawAliasRow{
		{Alias: "smart-chat", Kind: AliasKindRouted, DefaultRoute: strPtr("does-not-exist")},
	}, []rawRouteRow{
		{Alias: "smart-chat", Name: "general", Description: "d", Target: "llama-3-8b"},
	})
	if _, ok := aliases["smart-chat"]; ok {
		t.Error("expected a routed alias whose default_route is not among its routes to be dropped")
	}
}

// TestBuildAliases_OneInconsistentAliasDoesNotAffectOthers asserts the drop
// is per-alias — a broken routed alias must not take down a healthy static
// alias or a healthy routed alias loaded in the same batch.
func TestBuildAliases_OneInconsistentAliasDoesNotAffectOthers(t *testing.T) {
	aliasRows := []rawAliasRow{
		{Alias: "ok-static", Kind: AliasKindStatic, Target: strPtr("llama-3-8b")},
		{Alias: "broken-router", Kind: AliasKindRouted, DefaultRoute: strPtr("missing")},
		{Alias: "ok-router", Kind: AliasKindRouted, DefaultRoute: strPtr("general"), ClassifierKind: strPtr(ClassifierKindSemanticRouter)},
	}
	routeRows := []rawRouteRow{
		{Alias: "ok-router", Name: "general", Description: "d", Target: "llama-3-8b"},
	}

	aliases := buildAliases(aliasRows, routeRows)

	if _, ok := aliases["ok-static"]; !ok {
		t.Error("expected ok-static to survive")
	}
	if _, ok := aliases["broken-router"]; ok {
		t.Error("expected broken-router to be dropped")
	}
	def, ok := aliases["ok-router"]
	if !ok {
		t.Fatal("expected ok-router to survive")
	}
	if def.ClassifierKind != ClassifierKindSemanticRouter {
		t.Errorf("got %+v", def)
	}
}

// TestBuildAliases_LegacyModelClassifierKindCoercedToRules covers the
// migration-014 tolerance case: a routed alias row that still carries the
// removed classifier_kind='model' value (a gateway pod started before the
// api's migration lands, or against a not-yet-migrated database) must be
// treated as 'rules', not dropped.
func TestBuildAliases_LegacyModelClassifierKindCoercedToRules(t *testing.T) {
	aliasRows := []rawAliasRow{
		{Alias: "smart-chat", Kind: AliasKindRouted, DefaultRoute: strPtr("general"), ClassifierKind: strPtr("model")},
	}
	routeRows := []rawRouteRow{
		{Alias: "smart-chat", Name: "general", Description: "d", Target: "llama-3-8b"},
	}

	aliases := buildAliases(aliasRows, routeRows)

	def, ok := aliases["smart-chat"]
	if !ok {
		t.Fatal("expected smart-chat to survive with its classifier_kind coerced, not dropped")
	}
	if def.ClassifierKind != ClassifierKindRules {
		t.Errorf("ClassifierKind = %q, want %q", def.ClassifierKind, ClassifierKindRules)
	}
}

func strPtr(s string) *string { return &s }

func TestBuildAliases_CarriesOnRouteUnavailable(t *testing.T) {
	got := buildAliases(
		[]rawAliasRow{{Alias: "a", Kind: AliasKindRouted, DefaultRoute: strPtr("general"), ClassifierKind: strPtr("rules"), OnRouteUnavailable: OnRouteUnavailableRefuse}},
		[]rawRouteRow{{Alias: "a", Name: "general", Target: "m"}},
	)
	if got["a"].OnRouteUnavailable != OnRouteUnavailableRefuse {
		t.Errorf("OnRouteUnavailable = %q", got["a"].OnRouteUnavailable)
	}
}
