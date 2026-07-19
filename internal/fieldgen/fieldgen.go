// Package fieldgen renders compile-checked field references — one
// `<Model>Fields` struct var per store-tagged model, in a single
// chok_fields_gen.go beside the models — from a package directory's Go
// source. It is the engine behind `chok gen fields`; cmd/chok stays a
// thin shell over Scan+Render, the same shape as internal/docgen behind
// `chok docs gen`.
//
// The scan is deliberately syntax-level (go/parser, no type checking):
// regeneration must keep working while the package is mid-refactor and
// does not compile — the exact moment a model rename needs the file
// refreshed. Column-ness is therefore decided by a syntactic
// classification that mirrors GORM's parse outcome (see classifyColumn)
// instead of by go/types: builtins, local defined scalars, local Valuer
// structs and a known set of cross-package column types are generated;
// local plain-struct fields (GORM relations, DBName empty at runtime)
// are skipped with a warning; a cross-package type the scan cannot
// classify is a hard error pointing at `gorm:"type:..."` / serializer
// tags as the static proof, or at removing the store tag.
//
// The remaining blind spot is promotion: GORM lifts `store` tags out of
// anonymously embedded (exported) user structs and `gorm:"embedded"`
// fields, which a syntactic scan will not expand. Wherever the scan can
// verify a local embed carries tags it warns — including on a struct
// with no direct tags of its own, when it embeds a chok base and would
// therefore be a runtime model the generator cannot represent. A model
// whose only tags ride an embed from another package stays invisible;
// db.md documents that residual honestly.
//
// Public-name derivation mirrors store.tagDeclaredFields: the JSON name
// (first comma segment); when the field is hidden from JSON the GORM
// column — explicit `gorm:"column:..."` or the default NamingStrategy,
// which db/open.go pins as a framework invariant by never configuring a
// custom namer. The generated values are whitelist map keys, not SQL
// identifiers, so WithColumnAlias never invalidates them. The semantic
// latch in store/fieldgen_latch_test.go pins the two name
// implementations together against a real store.
package fieldgen

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"gorm.io/gorm/schema"
)

// GenFileName is the generated file's name, one per package directory —
// the chok_modules_gen.go naming family.
const GenFileName = "chok_fields_gen.go"

// Field is one generated struct entry.
type Field struct {
	GoName string // generated struct field name (the model's Go field name)
	Value  string // public field name — the whitelist map key
	Query  bool
	Update bool
	Base   bool // base-model trio contribution (query-only)
}

// Model is one store-tagged struct and its declared surface.
type Model struct {
	Name   string
	Fields []Field // declaration order; base trio appended
}

// Package is the scan result for one directory.
type Package struct {
	Name     string // package clause of the scanned files
	Models   []Model
	Warnings []string
}

// namer computes the default GORM column for fields hidden from JSON
// with no explicit column tag. db/open.go never sets a NamingStrategy,
// so the zero-value default is exactly what store.New's schema parse
// resolves DBName with.
var namer = schema.NamingStrategy{}

// baseTrio is the standard query surface every db.Model embedder gets,
// mirroring the literal keys in store.tagDeclaredFields. store.New
// rejects models without the base embed, so the trio is generated
// unconditionally (no embed detection needed).
var baseTrio = []Field{
	{GoName: "ID", Value: "id", Query: true, Base: true},
	{GoName: "CreatedAt", Value: "created_at", Query: true, Base: true},
	{GoName: "UpdatedAt", Value: "updated_at", Query: true, Base: true},
}

// dbImportPath identifies the chok base-model package so recognized
// anonymous embeds (db.Model and friends) do not trigger the
// unknown-embed warning.
const dbImportPath = "github.com/zynthara/chok/v2/db"

var knownBaseEmbeds = map[string]bool{
	"Model": true, "SoftDeleteModel": true,
	"OwnedModel": true, "OwnedSoftDeleteModel": true,
	"Owned": true,
}

// builtinScalars are the predeclared types GORM maps to a column by
// reflect kind.
var builtinScalars = map[string]bool{
	"bool": true, "string": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"uintptr": true, "byte": true, "rune": true,
	"float32": true, "float64": true,
}

// isKnownColumnType reports cross-package types the scan accepts as
// database columns without further proof: they either have a GORM
// special case (time.Time, gorm.DeletedAt) or implement driver.Valuer
// in their home package (database/sql Null*, gorm.io/datatypes).
func isKnownColumnType(importPath, name string) bool {
	switch importPath {
	case "time":
		return name == "Time"
	case "database/sql":
		return strings.HasPrefix(name, "Null")
	case "gorm.io/gorm":
		return name == "DeletedAt"
	case "gorm.io/datatypes":
		return true
	}
	return false
}

// typeInfo is the pass-1 index entry for a package-local type.
type typeInfo struct {
	strct    *ast.StructType // nil when the declaration is not a struct
	exported bool
}

// scanner holds the package-wide context pass 2 classifies against.
type scanner struct {
	types   map[string]typeInfo
	valuers map[string]bool // local types with a Value() (driver.Valuer-shaped) method
}

// Scan parses the directory's Go source and collects every top-level
// struct carrying at least one generatable `store:` tag. It honors the
// current platform's build constraints (go/build.MatchFile) and skips
// _test.go and *_gen.go files. Malformed tag values, duplicate public
// names mapping to different columns, statically unclassifiable field
// types, unparsable files, and pre-existing `<Model>Fields`
// declarations are hard errors — the same fail-loud posture as the
// runtime's construction panics. A directory with no tagged models
// returns a Package with an empty Models slice.
func Scan(dir string) (*Package, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("fieldgen: read dir %s: %w", dir, err)
	}

	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, "_gen.go") {
			continue
		}
		// Build-constraint filter: models behind //go:build or platform
		// suffixes follow the generating platform, exactly as a runtime
		// binary built here would see them. Ignored files (_foo.go,
		// .foo.go, mismatched constraints) never reach the parser.
		match, err := build.Default.MatchFile(dir, name)
		if err != nil {
			return nil, fmt.Errorf("fieldgen: %s: %w", filepath.Join(dir, name), err)
		}
		if !match {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("fieldgen: %w", err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		return &Package{}, nil
	}

	// Pass 1: package-wide indexes — local types (struct or not),
	// Valuer-shaped methods, and every top-level identifier (the
	// <Model>Fields conflict surface).
	sc := &scanner{types: map[string]typeInfo{}, valuers: map[string]bool{}}
	pkg := &Package{Name: files[0].Name.Name}
	topLevel := make(map[string]bool)
	for _, f := range files {
		if f.Name.Name != pkg.Name {
			return nil, fmt.Errorf("fieldgen: %s: mixed packages %q and %q in one directory", dir, pkg.Name, f.Name.Name)
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					topLevel[d.Name.Name] = true
					continue
				}
				if recv := receiverTypeName(d); recv != "" && isValuerShaped(d) {
					sc.valuers[recv] = true
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.ValueSpec:
						for _, n := range s.Names {
							topLevel[n.Name] = true
						}
					case *ast.TypeSpec:
						topLevel[s.Name.Name] = true
						st, _ := s.Type.(*ast.StructType)
						sc.types[s.Name.Name] = typeInfo{strct: st, exported: s.Name.IsExported()}
					}
				}
			}
		}
	}

	// Pass 2: scan structs with full package context.
	for _, f := range files {
		imports := fileImports(f)
		for _, decl := range f.Decls {
			d, ok := decl.(*ast.GenDecl)
			if !ok || d.Tok != token.TYPE {
				continue
			}
			for _, spec := range d.Specs {
				s, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := s.Type.(*ast.StructType)
				if !ok {
					continue
				}
				model, warns, err := sc.scanStruct(s.Name.Name, st, imports)
				if err != nil {
					return nil, err
				}
				if model != nil {
					pkg.Models = append(pkg.Models, *model)
				}
				pkg.Warnings = append(pkg.Warnings, warns...)
			}
		}
	}

	sort.Slice(pkg.Models, func(i, j int) bool { return pkg.Models[i].Name < pkg.Models[j].Name })
	for _, m := range pkg.Models {
		if sym := m.Name + "Fields"; topLevel[sym] {
			return nil, fmt.Errorf("fieldgen: package %s already declares %s — rename that declaration; the generated file owns the symbol", pkg.Name, sym)
		}
	}
	return pkg, nil
}

// scanStruct extracts the model declaration from one struct, or nil
// when the struct contributes no generatable field. Warnings can
// accompany a nil model: a struct that embeds a chok base and carries
// its whole tag surface inside promoted embeds is a runtime model the
// generator cannot represent, and must say so rather than stay silent.
func (sc *scanner) scanStruct(name string, st *ast.StructType, imports map[string]string) (*Model, []string, error) {
	var (
		fields []Field
		// Three warning buckets with different emission rules:
		// fieldWarns are actionable per-field diagnoses (an ignored
		// store tag) — always emitted; modelWarns are opaque-embed
		// notes only meaningful when the struct is a generated model;
		// promoWarns are verified/unverifiable promotions — emitted for
		// models, and for base-embedding structs even when nothing is
		// generated (the silent-model failure mode of review round-1).
		fieldWarns []string
		modelWarns []string
		promoWarns []string
		hasDBBase  bool
		queryCol   = map[string]string{}
		updCol     = map[string]string{}
	)
	dbImport := chokDBIdent(imports)

	addField := func(goName string, tag reflect.StructTag, gormSettings map[string]string) error {
		column := gormSettings["COLUMN"]
		if column == "" {
			column = namer.ColumnName("", goName)
		}
		value := publicName(tag.Get("json"), column)
		fld := Field{GoName: goName, Value: value}
		for _, rawFace := range strings.Split(tag.Get("store"), ",") {
			switch strings.TrimSpace(rawFace) {
			case "query":
				if err := addDeclared(queryCol, name, goName, value, column); err != nil {
					return err
				}
				fld.Query = true
			case "update":
				if err := addDeclared(updCol, name, goName, value, column); err != nil {
					return err
				}
				fld.Update = true
			default:
				return fmt.Errorf(
					"fieldgen: %s.%s: bad `store:%q` tag value %q — use \"query\", \"update\" or both (remove the tag to keep the field private)",
					name, goName, tag.Get("store"), strings.TrimSpace(rawFace))
			}
		}
		fields = append(fields, fld)
		return nil
	}

	for _, f := range st.Fields.List {
		tag := fieldTag(f)
		_, hasStoreTag := tag.Lookup("store")
		gormSettings := parseGormSettings(tag.Get("gorm"))

		// Anonymous fields: chok bases, promoted embeds, or — for a
		// named non-struct type — a regular column under the type name.
		if len(f.Names) == 0 {
			typ := unwrapType(f.Type)
			if isKnownBaseEmbed(typ, dbImport) {
				hasDBBase = true
				continue
			}
			if gormIgnored(gormSettings) {
				continue
			}
			if _, embedded := gormSettings["EMBEDDED"]; embedded {
				promoWarns = append(promoWarns, sc.promotionWarn(name, exprString(f.Type), typ)...)
				continue
			}
			switch t := typ.(type) {
			case *ast.Ident:
				info, local := sc.types[t.Name]
				switch {
				case local && info.strct == nil:
					// Embedded local defined scalar: a regular runtime
					// column named after the type (unexported types are
					// unexported fields — GORM skips those).
					if hasStoreTag && ast.IsExported(t.Name) {
						if err := addField(t.Name, tag, gormSettings); err != nil {
							return nil, nil, err
						}
					}
				case local && !info.exported:
					// Unexported embedded struct = unexported field:
					// GORM ignores the whole embed (verified against
					// the runtime), so silence is consistency.
				case local:
					promoWarns = append(promoWarns, sc.promotionWarn(name, exprString(f.Type), typ)...)
				default:
					modelWarns = append(modelWarns, opaqueEmbedWarn(name, f.Type))
				}
			case *ast.SelectorExpr:
				if hasStoreTag && selectorIsKnownColumn(t, imports) {
					if err := addField(t.Sel.Name, tag, gormSettings); err != nil {
						return nil, nil, err
					}
					continue
				}
				modelWarns = append(modelWarns, opaqueEmbedWarn(name, f.Type))
			default:
				modelWarns = append(modelWarns, opaqueEmbedWarn(name, f.Type))
			}
			continue
		}

		// Named fields. gorm:"embedded" wrappers promote their inner
		// tags at runtime whether or not the wrapper itself is tagged.
		if _, embedded := gormSettings["EMBEDDED"]; embedded && !gormIgnored(gormSettings) {
			for _, ident := range f.Names {
				if !ident.IsExported() {
					continue
				}
				promoWarns = append(promoWarns, sc.promotionWarn(name, ident.Name, unwrapType(f.Type))...)
				if hasStoreTag {
					fieldWarns = append(fieldWarns, fmt.Sprintf(
						"%s.%s: `store` tag on a gorm-embedded field is ignored at runtime — tag the embedded struct's own fields at top level instead", name, ident.Name))
				}
			}
			continue
		}
		if !hasStoreTag {
			continue
		}
		if gormIgnored(gormSettings) {
			// No column at runtime (DBName stays empty), so
			// tagDeclaredFields never sees this tag either.
			continue
		}
		for _, ident := range f.Names {
			if !ident.IsExported() {
				continue // GORM never maps unexported fields; the tag is dead
			}
			verdict, why := sc.classifyColumn(f.Type, gormSettings, imports)
			switch verdict {
			case colYes:
				if err := addField(ident.Name, tag, gormSettings); err != nil {
					return nil, nil, err
				}
			case colNo:
				fieldWarns = append(fieldWarns, fmt.Sprintf(
					"%s.%s: `store` tag ignored — %s is %s, not a database column (the runtime skips it too); remove the tag", name, ident.Name, exprString(f.Type), why))
			case colUnknown:
				return nil, nil, fmt.Errorf(
					"fieldgen: %s.%s: cannot statically decide whether %s is a database column — prove it with `gorm:\"type:...\"` or a serializer tag, or remove the `store` tag if it is a relation",
					name, ident.Name, exprString(f.Type))
			}
		}
	}

	if len(fields) == 0 {
		// Not a generatable model. Per-field diagnoses still surface;
		// and if the struct embeds a chok base with promoted tags, it
		// IS a runtime model whose whole surface the generator misses —
		// silence here is the failure mode review round-1 flagged, so
		// the promotion warnings fire even with nothing generated.
		// Opaque-embed notes stay model-only (a DTO wrapping a model
		// would otherwise warn on every scan).
		if hasDBBase {
			return nil, append(promoWarns, fieldWarns...), nil
		}
		return nil, fieldWarns, nil
	}

	warns := append(promoWarns, fieldWarns...)
	warns = append(warns, modelWarns...)

	goNames := make(map[string]bool, len(fields))
	for _, f := range fields {
		goNames[f.GoName] = true
	}
	for _, b := range baseTrio {
		if _, taken := queryCol[b.Value]; taken {
			continue // a declared field owns the public name — one symbol per key
		}
		if goNames[b.GoName] {
			warns = append(warns, fmt.Sprintf(
				"%s: base field %s skipped — a declared field already uses the Go name; the %q key stays usable as a plain string", name, b.GoName, b.Value))
			continue
		}
		fields = append(fields, b)
	}
	return &Model{Name: name, Fields: fields}, warns, nil
}

func opaqueEmbedWarn(model string, typ ast.Expr) string {
	return fmt.Sprintf(
		"%s: anonymous embed %s is opaque to the syntax-level scan — `store` tags inside it are not generated; lift them onto %s itself if it declares any",
		model, exprString(typ), model)
}

// promotionWarn reports a verified runtime promotion the generator
// cannot expand: an exported local struct (embedded anonymously or via
// gorm:"embedded") that transitively carries store tags.
func (sc *scanner) promotionWarn(model, fieldLabel string, typ ast.Expr) []string {
	ident, ok := typ.(*ast.Ident)
	if !ok {
		// Cross-package embedded target: tags inside are unverifiable.
		return []string{fmt.Sprintf(
			"%s: embedded %s is opaque to the syntax-level scan — `store` tags inside it are not generated; lift them onto %s itself if it declares any", model, fieldLabel, model)}
	}
	info, local := sc.types[ident.Name]
	if !local || info.strct == nil || !info.exported {
		return nil // unexported embeds never reach GORM; non-structs carry no promotion
	}
	if !sc.transitivelyTagged(ident.Name, map[string]bool{}) {
		return nil // verified tag-free — nothing the runtime could promote
	}
	return []string{fmt.Sprintf(
		"%s: embedded %s carries `store` tags the runtime will promote but the generator cannot see — lift the tags onto %s, or expect these references to be missing", model, fieldLabel, model)}
}

// transitivelyTagged reports whether a local exported struct carries a
// store tag the runtime would promote, walking nested local embeds.
func (sc *scanner) transitivelyTagged(typeName string, seen map[string]bool) bool {
	if seen[typeName] {
		return false
	}
	seen[typeName] = true
	info, ok := sc.types[typeName]
	if !ok || info.strct == nil {
		return false
	}
	for _, f := range info.strct.Fields.List {
		tag := fieldTag(f)
		_, tagged := tag.Lookup("store")
		gormSettings := parseGormSettings(tag.Get("gorm"))
		if gormIgnored(gormSettings) {
			continue
		}
		if len(f.Names) == 0 {
			if tagged {
				return true // tagged embedded scalar (or deeper struct — either way promoted)
			}
			if ident, ok := unwrapType(f.Type).(*ast.Ident); ok {
				if info := sc.types[ident.Name]; info.strct != nil && info.exported && sc.transitivelyTagged(ident.Name, seen) {
					return true
				}
			}
			continue
		}
		exported := false
		for _, ident := range f.Names {
			if ident.IsExported() {
				exported = true
			}
		}
		if !exported {
			continue
		}
		if tagged {
			return true
		}
		if _, embedded := gormSettings["EMBEDDED"]; embedded {
			if ident, ok := unwrapType(f.Type).(*ast.Ident); ok {
				if info := sc.types[ident.Name]; info.strct != nil && sc.transitivelyTagged(ident.Name, seen) {
					return true
				}
			}
		}
	}
	return false
}

type colVerdict int

const (
	colYes colVerdict = iota
	colNo
	colUnknown
)

// classifyColumn is the syntactic mirror of "does GORM give this field
// a DBName": explicit gorm type/serializer settings prove a column for
// any Go type; builtins, []byte, local defined scalars, local Valuer
// structs and the known cross-package set are columns; local plain
// structs, slices, maps and friends are relations or unsupported (no
// DBName — the runtime skips their store tag); anything else is
// undecidable and must fail loud rather than guess.
func (sc *scanner) classifyColumn(typ ast.Expr, gormSettings map[string]string, imports map[string]string) (colVerdict, string) {
	if gormSettings["TYPE"] != "" || gormSettings["SERIALIZER"] != "" {
		return colYes, "" // explicit column proof, any type goes
	}
	switch t := unwrapType(typ).(type) {
	case *ast.Ident:
		if info, local := sc.types[t.Name]; local {
			if info.strct == nil {
				return colYes, "" // defined scalar: GORM maps by kind
			}
			if sc.valuers[t.Name] {
				return colYes, "" // driver.Valuer struct: column by value
			}
			return colNo, "a plain struct (a GORM relation)"
		}
		if builtinScalars[t.Name] {
			return colYes, ""
		}
		return colUnknown, ""
	case *ast.SelectorExpr:
		if selectorIsKnownColumn(t, imports) {
			return colYes, ""
		}
		return colUnknown, ""
	case *ast.ArrayType:
		if elem, ok := t.Elt.(*ast.Ident); ok && (elem.Name == "byte" || elem.Name == "uint8") && t.Len == nil {
			return colYes, "" // []byte is a bytes column
		}
		return colNo, "a slice or array (a relation, or unsupported without a serializer)"
	case *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.InterfaceType, *ast.StructType:
		return colNo, "not a scalar type (no column without a serializer)"
	default:
		return colUnknown, ""
	}
}

// addDeclared mirrors store.addDeclaredField: one public name per face
// may only map to one column.
func addDeclared(m map[string]string, model, goName, value, column string) error {
	if existing, ok := m[value]; ok && existing != column {
		return fmt.Errorf(
			"fieldgen: %s.%s: declared field name %q maps to columns %q and %q — rename the JSON tag or drop one declaration",
			model, goName, value, existing, column)
	}
	m[value] = column
	return nil
}

// publicName mirrors store.storeTagFieldName: the JSON name's first
// comma segment when visible, the GORM column otherwise.
func publicName(jsonTag, column string) string {
	name, _, _ := strings.Cut(jsonTag, ",")
	if name == "" || name == "-" {
		return column
	}
	return name
}

// parseGormSettings is the subset of gorm/schema.ParseTagSetting the
// scanner needs: semicolon-separated `key:value` pairs with upper-cased
// keys; a bare key maps to itself (so `gorm:"-"` yields {"-": "-"}).
// Backslash-escaped separators are not supported — column names never
// contain them.
func parseGormSettings(tag string) map[string]string {
	settings := map[string]string{}
	for _, part := range strings.Split(tag, ";") {
		k, v, hasValue := strings.Cut(part, ":")
		k = strings.TrimSpace(strings.ToUpper(k))
		if k == "" {
			continue
		}
		if hasValue {
			settings[k] = v
		} else {
			settings[k] = k
		}
	}
	return settings
}

// gormIgnored mirrors the runtime `-` handling: `gorm:"-"` and
// `gorm:"-:all"` erase the column (DBName empty), `-:migration` keeps
// it.
func gormIgnored(settings map[string]string) bool {
	v, ok := settings["-"]
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "-", "all":
		return true
	}
	return false
}

// fieldTag decodes the field's raw tag literal; absent or malformed
// tags read as empty (the compiler owns the malformed-literal
// complaint).
func fieldTag(f *ast.Field) reflect.StructTag {
	if f.Tag == nil {
		return ""
	}
	raw, err := strconv.Unquote(f.Tag.Value)
	if err != nil {
		return ""
	}
	return reflect.StructTag(raw)
}

// fileImports maps each import's file-local identifier to its path.
// Dot and blank imports are dropped (a dot-imported model package is
// pathological; its types classify as unknown and fail loud).
func fileImports(f *ast.File) map[string]string {
	m := make(map[string]string, len(f.Imports))
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		if imp.Name != nil {
			if n := imp.Name.Name; n != "_" && n != "." {
				m[n] = p
			}
			continue
		}
		m[defaultImportIdent(p)] = p
	}
	return m
}

// defaultImportIdent guesses the package identifier from the import
// path: the last segment, or the one before a /vN major-version
// suffix. Exact enough for the known-set paths the scanner matches.
func defaultImportIdent(importPath string) string {
	base := path.Base(importPath)
	if len(base) > 1 && base[0] == 'v' && strings.TrimLeft(base[1:], "0123456789") == "" {
		base = path.Base(path.Dir(importPath))
	}
	return base
}

// chokDBIdent resolves the file-local identifier of the chok db
// package; empty when not imported.
func chokDBIdent(imports map[string]string) string {
	for ident, p := range imports {
		if p == dbImportPath {
			return ident
		}
	}
	return ""
}

func selectorIsKnownColumn(sel *ast.SelectorExpr, imports map[string]string) bool {
	x, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return isKnownColumnType(imports[x.Name], sel.Sel.Name)
}

// unwrapType strips pointers and generic instantiations down to the
// named type expression.
func unwrapType(expr ast.Expr) ast.Expr {
	for {
		switch t := expr.(type) {
		case *ast.StarExpr:
			expr = t.X
		case *ast.IndexExpr:
			expr = t.X
		case *ast.IndexListExpr:
			expr = t.X
		default:
			return expr
		}
	}
}

func isKnownBaseEmbed(expr ast.Expr, dbImport string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && dbImport != "" && x.Name == dbImport && knownBaseEmbeds[sel.Sel.Name]
}

// receiverTypeName returns the base type name of a method receiver.
func receiverTypeName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) != 1 {
		return ""
	}
	if ident, ok := unwrapType(d.Recv.List[0].Type).(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// isValuerShaped loosely matches `func (T) Value() (driver.Value,
// error)` — name, zero params, two results. Good enough syntactically:
// a Value method of that shape on a model-adjacent struct is the
// database-value contract.
func isValuerShaped(d *ast.FuncDecl) bool {
	return d.Name.Name == "Value" &&
		(d.Type.Params == nil || len(d.Type.Params.List) == 0) &&
		d.Type.Results != nil && len(d.Type.Results.List) == 2
}

func exprString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return exprString(e.X) + "." + e.Sel.Name
	case *ast.StarExpr:
		return "*" + exprString(e.X)
	case *ast.IndexExpr:
		return exprString(e.X) + "[...]"
	case *ast.ArrayType:
		return "[]" + exprString(e.Elt)
	case *ast.MapType:
		return "map[" + exprString(e.Key) + "]" + exprString(e.Value)
	default:
		return fmt.Sprintf("%T", expr)
	}
}

// Render emits the generated file for a non-empty scan, gofmt-formatted
// and byte-stable: same source in, same bytes out.
func Render(pkg *Package) ([]byte, error) {
	if len(pkg.Models) == 0 {
		return nil, fmt.Errorf("fieldgen: nothing to render — no store-tagged models")
	}
	var b strings.Builder
	b.WriteString("// Code generated by chok gen fields; DO NOT EDIT.\n")
	b.WriteString("// Source: `store` tags on this package's model structs — rerun\n")
	b.WriteString("// `chok gen fields` after adding, renaming or removing tagged fields.\n\n")
	fmt.Fprintf(&b, "package %s\n\n", pkg.Name)

	for i, m := range pkg.Models {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "// %sFields enumerates %s's declared field surface (`store` tags) as\n", m.Name, m.Name)
		b.WriteString("// compile-checked references. Values are the public field names the\n")
		b.WriteString("// store's whitelists key on; they are stable under WithColumnAlias.\n")
		fmt.Fprintf(&b, "var %sFields = struct {\n", m.Name)
		writeGroups(&b, m.Fields, func(f Field) string {
			return fmt.Sprintf("\t%s string // %s\n", f.GoName, faceComment(f))
		})
		b.WriteString("}{\n")
		writeGroups(&b, m.Fields, func(f Field) string {
			return fmt.Sprintf("\t%s: %q,\n", f.GoName, f.Value)
		})
		b.WriteString("}\n")
	}

	src, err := format.Source([]byte(b.String()))
	if err != nil {
		return nil, fmt.Errorf("fieldgen: format generated code: %w", err)
	}
	return src, nil
}

// writeGroups renders the declared fields, a blank separator, then the
// base trio — either group may be absent.
func writeGroups(b *strings.Builder, fields []Field, line func(Field) string) {
	wroteDeclared := false
	for _, f := range fields {
		if f.Base {
			continue
		}
		b.WriteString(line(f))
		wroteDeclared = true
	}
	wroteBlank := !wroteDeclared
	for _, f := range fields {
		if !f.Base {
			continue
		}
		if !wroteBlank {
			b.WriteString("\n")
			wroteBlank = true
		}
		b.WriteString(line(f))
	}
}

func faceComment(f Field) string {
	switch {
	case f.Base && f.GoName == "ID":
		return "base model, query-only (resolves to the rid column)"
	case f.Base:
		return "base model, query-only"
	case f.Query && f.Update:
		return "faces: query, update"
	case f.Query:
		return "faces: query"
	default:
		return "faces: update"
	}
}
