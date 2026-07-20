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
// instead of by go/types: builtins, exact driver.Valuer / GormDataType
// implementers and a known set of cross-package column types are
// generated, with local defined types resolved through their underlying
// shape (a defined scalar is a column, a defined slice of structs is a
// has-many). Struct-shaped relations (DBName empty at runtime) are
// skipped with a warning; shapes GORM cannot schema-parse AT ALL —
// containers of non-struct elements, maps, chans, funcs, interfaces,
// uintptr/complex, an empty GormDataType — abort the runtime model
// build (verified against the pinned GORM), so on a runtime model they
// are hard errors whether named or anonymous, tagged or not; only the
// gorm:"-" family or fully closed permissions (`->:false;<-:false`)
// keep such a field inert. A cross-package type the scan cannot
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
// reflect kind. uintptr is deliberately absent: the pinned GORM version
// has no mapping for it and schema parsing fails, so the scan must fail
// loud too (review round-4).
var builtinScalars = map[string]bool{
	"bool": true, "string": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"byte": true, "rune": true,
	"float32": true, "float64": true,
}

// fatalScalars are predeclared types GORM's kind switch has no mapping
// for (verified against v1.31.2): their DataType stays empty, the
// relation gate picks the field up and getOrParse aborts the whole
// model with ErrUnsupportedDataType — named or anonymous alike. The
// predeclared interface types land on the same fate through the
// interface kind.
var fatalScalars = map[string]bool{
	"uintptr": true, "complex64": true, "complex128": true,
	"any": true, "error": true,
}

// datatypesStorage is the explicit storage-type whitelist for
// gorm.io/datatypes — the package also exports query-expression types
// (JSONQueryExpression and friends) that are neither Valuers nor
// columns, so a whole-package rule would mint dead references (review
// round-4).
var datatypesStorage = map[string]bool{
	"JSON": true, "JSONMap": true, "JSONSlice": true, "JSONType": true,
	"Null": true, "NullBool": true, "NullByte": true, "NullFloat64": true,
	"NullInt16": true, "NullInt32": true, "NullInt64": true,
	"NullString": true, "NullTime": true,
	"Date": true, "Time": true, "UUID": true, "BinUUID": true, "URL": true,
}

// isKnownColumnType reports cross-package types the scan accepts as
// database columns without further proof: they either have a GORM
// special case (time.Time, gorm.DeletedAt) or implement driver.Valuer
// in their home package (database/sql Null*, the gorm.io/datatypes
// storage types).
func isKnownColumnType(importPath, name string) bool {
	switch importPath {
	case "time":
		return name == "Time"
	case "database/sql":
		return strings.HasPrefix(name, "Null")
	case "gorm.io/gorm":
		return name == "DeletedAt"
	case "gorm.io/datatypes":
		return datatypesStorage[name]
	}
	return false
}

// typeInfo is the pass-1 index entry for a package-local type.
type typeInfo struct {
	decl       ast.Expr // the TypeSpec's type expression — the underlying shape
	isAlias    bool     // `type A = B`: full identity, methods included
	exported   bool
	typeParams []string          // generic parameter names, declaration order
	imports    map[string]string // the declaring file's imports, for resolving decl
}

// methodState describes one declared method relevant to column
// proving.
type methodState struct {
	exact   bool   // signature matches the interface exactly (aliases resolved)
	literal string // GormDataType only: the single-return string literal
	dynamic bool   // GormDataType only: body is not a single literal return
}

// scanner holds the package-wide context pass 2 classifies against.
type scanner struct {
	types map[string]typeInfo
	// methods[typeName][memberName] — only the two column-proving
	// names are indexed ("Value", "GormDataType"), with receiver names
	// canonicalized through alias chains. Presence with exact=false
	// matters too: a same-named method with the wrong signature
	// shadows deeper promotions (Go selector rules, review round-4).
	methods map[string]map[string]methodState
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

	// Pass 1: package-wide indexes — local types with their underlying
	// shape and declaring-file imports, raw method declarations, and
	// every top-level identifier (the <Model>Fields conflict surface).
	// Methods are resolved AFTER all types are known: their signature
	// types may ride aliases declared in other files (review round-4).
	sc := &scanner{types: map[string]typeInfo{}, methods: map[string]map[string]methodState{}}
	pkg := &Package{Name: files[0].Name.Name}
	topLevel := make(map[string]bool)
	type rawMethod struct {
		recv    string
		decl    *ast.FuncDecl
		imports map[string]string
	}
	var rawMethods []rawMethod
	for _, f := range files {
		if f.Name.Name != pkg.Name {
			return nil, fmt.Errorf("fieldgen: %s: mixed packages %q and %q in one directory", dir, pkg.Name, f.Name.Name)
		}
		imports := fileImports(f)
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					topLevel[d.Name.Name] = true
					continue
				}
				if recv := receiverTypeName(d); recv != "" &&
					(d.Name.Name == "Value" || d.Name.Name == "GormDataType") {
					rawMethods = append(rawMethods, rawMethod{recv: recv, decl: d, imports: imports})
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
						sc.types[s.Name.Name] = typeInfo{
							decl:       s.Type,
							isAlias:    s.Assign.IsValid(),
							exported:   s.Name.IsExported(),
							typeParams: typeParamNames(s),
							imports:    imports,
						}
					}
				}
			}
		}
	}
	// Pass 1.5: resolve methods. Receivers written as aliases attach to
	// the aliased type; signature types resolve through alias chains.
	for _, m := range rawMethods {
		recv := sc.aliasTerminal(m.recv).name
		state := methodState{}
		switch m.decl.Name.Name {
		case "Value":
			state.exact = sc.isValuerMethod(m.decl, m.imports)
		case "GormDataType":
			if sc.isGormDataTypeMethod(m.decl, m.imports) {
				state.exact = true
				state.literal, state.dynamic = methodStringLiteral(m.decl)
			}
		}
		if sc.methods[recv] == nil {
			sc.methods[recv] = map[string]methodState{}
		}
		sc.methods[recv][m.decl.Name.Name] = state
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
		// anonFatal records the first anonymous (or gorm-embedded) shape
		// GORM cannot expand: the EMBEDDED branch aborts on it wherever
		// the struct is parsed — directly or inside another embed — so
		// any generated-or-based struct must fail the scan rather than
		// warn (review rounds 6-7). namedFatal records the first NAMED
		// DataType-less shape the relation gate would abort on; embedded
		// sub-schemas skip relation parsing, so this one only fires for
		// base-carrying structs (runtime models by contract). Plain DTOs
		// never meet a schema parse and stay silent either way.
		anonFatal  error
		namedFatal error
		// baseFatal records a chok base embed disabled by its own gorm
		// tag (gorm:"-" family or fully closed permissions): GORM will
		// not expand it, so the promoted base fields (rid, id, the
		// timestamps) will not exist and store.New fails on the missing
		// RID — generating base references would be dead symbols
		// (review round-8).
		baseFatal error
		hasDBBase bool
		// carriedBase: a chok base reached through expanding local
		// struct embeds (review round-8). It arms the FATALITY gates —
		// the struct is a runtime model — but deliberately not the
		// promotion warnings, whose direct-base gate keeps DTOs that
		// wrap a model quiet (the round-1 contract).
		carriedBase bool
		queryCol    = map[string]string{}
		updCol      = map[string]string{}
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

		// Anonymous fields: chok bases, promoted embeds, or — when the
		// type itself is column-shaped (a defined scalar, a
		// driver.Valuer struct, sql.NullString, ...) — a regular
		// column under the type's name.
		if len(f.Names) == 0 {
			typ := unwrapType(f.Type)
			if isKnownBaseEmbed(typ, dbImport) {
				// The base only counts if GORM actually expands it: its
				// own gorm tag can disable the embed entirely (review
				// round-8), leaving the struct without rid/id at runtime.
				if gormIgnored(gormSettings) || permsAllClosed(gormSettings) {
					if baseFatal == nil {
						baseFatal = fmt.Errorf(
							"fieldgen: %s: the chok base embed %s is disabled by its gorm tag — GORM will not expand it, the promoted base fields (rid, id, timestamps) will not exist and store.New fails; remove the tag",
							name, exprString(f.Type))
					}
					continue
				}
				hasDBBase = true
				continue
			}
			if gormIgnored(gormSettings) {
				continue
			}
			closed := permsAllClosed(gormSettings)
			if _, embedded := gormSettings["EMBEDDED"]; embedded {
				// The EMBEDDED tag forces GORM's embedded branch whatever
				// the permission tags say (the tag arm has no perms gate;
				// review round-7 finding 2): a struct target expands, a
				// scalar target is a no-op (the field stays a plain
				// column — classify it below), and container or fatal
				// kinds abort schema parsing — even a byte slice.
				switch sc.shapeKindOf(f.Type, imports, nil, map[string]bool{}, false) {
				case shapeContainer, shapeFatal:
					err := fmt.Errorf(
						"fieldgen: %s.%s: `gorm:\"embedded\"` on %s — not a struct, so GORM aborts schema parsing (invalid embedded struct); drop the embedded tag or restructure the field",
						name, embedFieldName(typ), exprString(f.Type))
					if hasStoreTag {
						return nil, nil, err
					}
					if anonFatal == nil {
						anonFatal = err
					}
					continue
				case shapeScalar:
					// inert tag: classify like any anonymous scalar below
				case shapeStruct:
					promoWarns = append(promoWarns, sc.promotionWarn(name, exprString(f.Type), typ, true)...)
					if ident, ok := typ.(*ast.Ident); ok {
						ae, ne := sc.promotedFatal(ident.Name, map[string]bool{})
						if anonFatal == nil {
							anonFatal = ae
						}
						if namedFatal == nil {
							namedFatal = ne
						}
					}
					continue
				default:
					// Opaque target: a struct would promote, a scalar
					// would keep the column, other kinds abort — with a
					// store tag riding on the answer, guessing is not an
					// option (review round-8).
					if hasStoreTag {
						return nil, nil, fmt.Errorf(
							"fieldgen: %s.%s: cannot statically decide what `gorm:\"embedded\"` does to %s (its underlying kind is not visible) — remove the store tag, or restructure the field",
							name, embedFieldName(typ), exprString(f.Type))
					}
					promoWarns = append(promoWarns, sc.promotionWarn(name, exprString(f.Type), typ, true)...)
					continue
				}
			}
			// The embedded field's name is the type's base name; an
			// unexported one is an unexported field GORM skips
			// (verified against the runtime).
			fieldName := embedFieldName(typ)
			if fieldName == "" {
				modelWarns = append(modelWarns, opaqueEmbedWarn(name, f.Type))
				continue
			}
			if !ast.IsExported(fieldName) {
				continue
			}
			// Classification sees the ORIGINAL expression — generic
			// arguments intact (review round-5: an embedded Bytes[byte]
			// IS a bytes column); the unwrapped form serves only the
			// name/base/promotion duties above.
			verdict, why := sc.classifyAnonymous(f.Type, gormSettings, imports)
			switch verdict {
			case colYes:
				// Column-shaped embed: a field under the type's name.
				// Untagged it is just a private column — silent.
				if hasStoreTag {
					if err := addField(fieldName, tag, gormSettings); err != nil {
						return nil, nil, err
					}
				}
			case colNo:
				if hasStoreTag {
					// The tag on the embed line itself is dead — any
					// promoted inner tags are diagnosed separately.
					fieldWarns = append(fieldWarns, fmt.Sprintf(
						"%s.%s: `store` tag ignored — %s is %s, not a database column (the runtime skips it too); remove the tag", name, fieldName, exprString(f.Type), why))
				}
				if !closed && why == reasonEmbeddedStruct {
					// The embed EXPANDS (fully closed permissions keep the
					// branch shut, and a Valuer-relation never enters it):
					// its promoted fields join THIS struct's schema, so a
					// transitively carried chok base makes this a runtime
					// model, and any fatal shape inside aborts it exactly
					// like a direct field (review round-8).
					promoWarns = append(promoWarns, sc.promotionWarn(name, exprString(f.Type), typ, true)...)
					if ident, ok := typ.(*ast.Ident); ok {
						if sc.carriesBase(ident.Name, map[string]bool{}) {
							carriedBase = true
						}
						ae, ne := sc.promotedFatal(ident.Name, map[string]bool{})
						if anonFatal == nil {
							anonFatal = ae
						}
						if namedFatal == nil {
							namedFatal = ne
						}
					}
				}
			case colHardNo:
				if closed {
					// Both fatal paths are perms-gated for plain
					// anonymous fields: fully closed means inert at
					// runtime, so the shape aborts nothing — the store
					// tag is dead at most (review round-7 finding 5).
					if hasStoreTag {
						fieldWarns = append(fieldWarns, fmt.Sprintf(
							"%s.%s: `store` tag ignored — %s has fully closed permissions and no column at runtime; remove the tag", name, fieldName, exprString(f.Type)))
					}
					continue
				}
				// The runtime does NOT skip this shape: the embedded
				// branch (or, DataType-less, the relation gate) aborts
				// model building. A store tag declares model intent —
				// fail now; untagged, fail only if the struct proves to
				// be a runtime model after the walk.
				err := fmt.Errorf(
					"fieldgen: %s.%s: anonymous embed %s is %s — GORM aborts schema parsing on it (invalid embedded struct / unsupported data type), the model cannot be built; name the field and prove a column with a serializer, exclude it (`gorm:\"-\"`), or close its permissions (`gorm:\"->:false;<-:false\"`)",
					name, fieldName, exprString(f.Type), why)
				if hasStoreTag {
					return nil, nil, err
				}
				if anonFatal == nil {
					anonFatal = err
				}
			default:
				if hasStoreTag {
					// Tagged but undecidable (a dynamic GormDataType, an
					// opaque cross-package type, ...): claiming either
					// column or embed would be a guess — fail loud.
					return nil, nil, fmt.Errorf(
						"fieldgen: %s.%s: cannot statically decide whether the embedded %s is a database column — remove the `store` tag, or restructure the embed",
						name, fieldName, exprString(f.Type))
				}
				modelWarns = append(modelWarns, opaqueEmbedWarn(name, f.Type))
			}
			continue
		}

		// Named fields. gorm:"embedded" wrappers force GORM's embedded
		// branch whatever the target kind and whatever the permission
		// tags say (review round-7 finding 2): struct targets promote
		// their inner tags, a scalar target makes the tag a no-op (the
		// field stays a plain column and classifies below), and
		// container or fatal kinds abort schema parsing.
		if _, embedded := gormSettings["EMBEDDED"]; embedded && !gormIgnored(gormSettings) {
			if kind := sc.shapeKindOf(f.Type, imports, nil, map[string]bool{}, false); kind != shapeScalar {
				for _, ident := range f.Names {
					if !ident.IsExported() {
						continue
					}
					switch kind {
					case shapeContainer, shapeFatal:
						err := fmt.Errorf(
							"fieldgen: %s.%s: `gorm:\"embedded\"` on %s — not a struct, so GORM aborts schema parsing (invalid embedded struct); drop the embedded tag or use a serializer instead",
							name, ident.Name, exprString(f.Type))
						if hasStoreTag {
							return nil, nil, err
						}
						if anonFatal == nil {
							anonFatal = err
						}
					case shapeStruct:
						promoWarns = append(promoWarns, sc.promotionWarn(name, ident.Name, unwrapType(f.Type), false)...)
						if hasStoreTag {
							fieldWarns = append(fieldWarns, fmt.Sprintf(
								"%s.%s: `store` tag on a gorm-embedded field is ignored at runtime — tag the embedded struct's own fields at top level instead", name, ident.Name))
						}
						if tIdent, ok := unwrapType(f.Type).(*ast.Ident); ok {
							ae, ne := sc.promotedFatal(tIdent.Name, map[string]bool{})
							if anonFatal == nil {
								anonFatal = ae
							}
							if namedFatal == nil {
								namedFatal = ne
							}
						}
					default:
						// Opaque target: promoting, keeping the column or
						// aborting all depend on the invisible kind — a
						// riding store tag cannot be honored by guessing
						// (review round-8).
						if hasStoreTag {
							return nil, nil, fmt.Errorf(
								"fieldgen: %s.%s: cannot statically decide what `gorm:\"embedded\"` does to %s (its underlying kind is not visible) — remove the store tag, or restructure the field",
								name, ident.Name, exprString(f.Type))
						}
						promoWarns = append(promoWarns, sc.promotionWarn(name, ident.Name, unwrapType(f.Type), false)...)
					}
				}
				continue
			}
		}
		if gormIgnored(gormSettings) {
			// No column at runtime (DBName stays empty), so
			// tagDeclaredFields never sees this tag either.
			continue
		}
		closed := permsAllClosed(gormSettings)
		for _, ident := range f.Names {
			if !ident.IsExported() {
				continue // GORM never maps unexported fields; the tag is dead
			}
			verdict, why := sc.classifyColumn(f.Type, gormSettings, imports)
			if !hasStoreTag {
				// Untagged exported fields still abort the whole model
				// when their shape has no schema mapping: the relation
				// gate feeds every DataType-less field to getOrParse
				// (review round-7). Only the permission algebra keeps
				// such a field inert.
				if verdict == colHardNo && !closed && namedFatal == nil {
					namedFatal = fmt.Errorf(
						"fieldgen: %s.%s: %s is %s — GORM aborts schema parsing on it (unsupported data type) and the model cannot be built; prove a column with a serializer or `gorm:\"type:...\"` tag, or exclude the field (`gorm:\"-\"` or `gorm:\"->:false;<-:false\"`)",
						name, ident.Name, exprString(f.Type), why)
				}
				continue
			}
			switch verdict {
			case colYes:
				if err := addField(ident.Name, tag, gormSettings); err != nil {
					return nil, nil, err
				}
			case colNo:
				fieldWarns = append(fieldWarns, fmt.Sprintf(
					"%s.%s: `store` tag ignored — %s is %s, not a database column (the runtime skips it too); remove the tag", name, ident.Name, exprString(f.Type), why))
			case colHardNo:
				if closed {
					// Inert at runtime: no relation gate, no column —
					// the tag is dead, not fatal (round-7 finding 5).
					fieldWarns = append(fieldWarns, fmt.Sprintf(
						"%s.%s: `store` tag ignored — %s has fully closed permissions and no column at runtime; remove the tag", name, ident.Name, exprString(f.Type)))
					continue
				}
				return nil, nil, fmt.Errorf(
					"fieldgen: %s.%s: %s is %s — GORM aborts schema parsing on it (unsupported data type) and the model cannot be built; prove a column with a serializer or `gorm:\"type:...\"` tag, or exclude the field (`gorm:\"-\"` or `gorm:\"->:false;<-:false\"`)",
					name, ident.Name, exprString(f.Type), why)
			case colUnknown:
				return nil, nil, fmt.Errorf(
					"fieldgen: %s.%s: cannot statically decide whether %s is a database column — prove it with `gorm:\"type:...\"` or a serializer tag, or remove the `store` tag if it is a relation",
					name, ident.Name, exprString(f.Type))
			}
		}
	}

	if baseFatal != nil && len(fields) > 0 {
		// The struct generates references, but its disabled base means
		// the runtime model can never be built by store.New — the base
		// trio would be dead symbols (review round-8).
		return nil, nil, baseFatal
	}
	if anonFatal != nil && (hasDBBase || carriedBase || len(fields) > 0) {
		// The struct IS a runtime model or an embeddable with a
		// generated surface, and GORM's embedded branch aborts on the
		// fatal shape wherever the struct gets parsed — directly or as
		// an embed. Generating (or warning) here would whitelist fields
		// of a model that cannot be built (review rounds 6-7).
		return nil, nil, anonFatal
	}
	if namedFatal != nil && (hasDBBase || carriedBase) {
		// Named DataType-less shapes meet the relation gate on a DIRECT
		// parse — and promoted fields RE-ENTER the outer gate when their
		// struct is expanded as an embed (review round-8), which is why
		// promotedFatal contributes here too. The gate arms on a chok
		// base, direct or carried through expanding embeds: those
		// structs are runtime models by contract (store.New requires
		// the base), and their schema parse WILL abort (latched). A
		// base-less embeddable stays legal — its own direct parse never
		// happens through the store, and the model that embeds it
		// reports the fatality itself.
		return nil, nil, namedFatal
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
// cannot expand: a local struct-shaped type (embedded anonymously or
// via gorm:"embedded") that transitively carries store tags. For
// anonymous embeds the TYPE name is the field name, so an unexported
// type means an unexported field GORM skips; a named gorm-embedded
// wrapper promotes regardless of its target type's exportedness — GORM
// checks the field name, not the type (review round-2).
func (sc *scanner) promotionWarn(model, fieldLabel string, typ ast.Expr, anonymous bool) []string {
	ident, ok := typ.(*ast.Ident)
	if !ok {
		// Cross-package embedded target: tags inside are unverifiable.
		return []string{fmt.Sprintf(
			"%s: embedded %s is opaque to the syntax-level scan — `store` tags inside it are not generated; lift them onto %s itself if it declares any", model, fieldLabel, model)}
	}
	info, local := sc.types[ident.Name]
	if !local {
		return nil // undeclared identifier: the compiler owns this
	}
	if anonymous && !info.exported {
		return nil // unexported field name — GORM skips the embed entirely
	}
	if sc.underlyingStruct(ident.Name, map[string]bool{}) == nil {
		return nil // not struct-shaped: nothing to promote
	}
	if !sc.transitivelyTagged(ident.Name, map[string]bool{}) {
		return nil // verified tag-free — nothing the runtime could promote
	}
	return []string{fmt.Sprintf(
		"%s: embedded %s carries `store` tags the runtime will promote but the generator cannot see — lift the tags onto %s, or expect these references to be missing", model, fieldLabel, model)}
}

// underlyingStructWithImports resolves a local type name through type
// chains (`type B A`, aliases included — shape survives both) to its
// struct declaration and that declaration's file imports, when the
// underlying shape is a struct.
func (sc *scanner) underlyingStructWithImports(name string, seen map[string]bool) (*ast.StructType, map[string]string) {
	if seen[name] {
		return nil, nil
	}
	seen[name] = true
	info, ok := sc.types[name]
	if !ok {
		return nil, nil
	}
	switch t := unwrapType(info.decl).(type) {
	case *ast.StructType:
		return t, info.imports
	case *ast.Ident:
		return sc.underlyingStructWithImports(t.Name, seen)
	}
	return nil, nil
}

func (sc *scanner) underlyingStruct(name string, seen map[string]bool) *ast.StructType {
	st, _ := sc.underlyingStructWithImports(name, seen)
	return st
}

// transitivelyTagged reports whether a local struct-shaped type carries
// a store tag the runtime would promote, walking nested local embeds.
func (sc *scanner) transitivelyTagged(typeName string, seen map[string]bool) bool {
	if seen[typeName] {
		return false
	}
	seen[typeName] = true
	st := sc.underlyingStruct(typeName, map[string]bool{})
	if st == nil {
		return false
	}
	for _, f := range st.Fields.List {
		tag := fieldTag(f)
		_, tagged := tag.Lookup("store")
		gormSettings := parseGormSettings(tag.Get("gorm"))
		if gormIgnored(gormSettings) {
			continue
		}
		if len(f.Names) == 0 {
			if ident, ok := unwrapType(f.Type).(*ast.Ident); ok {
				if !sc.types[ident.Name].exported && !ast.IsExported(ident.Name) {
					continue // unexported embedded field: GORM skips it
				}
				if tagged {
					return true // tagged embedded scalar or struct — promoted either way
				}
				if sc.transitivelyTagged(ident.Name, seen) {
					return true
				}
				continue
			}
			if tagged {
				return true
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
			if ident, ok := unwrapType(f.Type).(*ast.Ident); ok && sc.transitivelyTagged(ident.Name, seen) {
				return true
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
	// colHardNo: the shape has no schema mapping at all, so model
	// building FAILS at runtime — it is not a skipped relation. Two
	// runtime paths produce this (review rounds 6-7, latched against
	// gorm v1.31.2): anonymous fields of non-struct kind hit the
	// EMBEDDED branch's "invalid embedded struct" switch, and any
	// DataType-less field whose terminal type is not a struct hits the
	// relation gate, whose getOrParse aborts with ErrUnsupportedDataType
	// — named or anonymous alike.
	colHardNo
)

// reasonPlainStruct is the colNo reason classifyShape gives a struct
// shape. classifyAnonymous keys on it: a struct-shaped embed expands at
// runtime, while every other non-column shape aborts schema parsing.
const reasonPlainStruct = "a plain struct (a GORM relation)"

// reasonEmbeddedStruct is classifyAnonymous's expanding-embed verdict;
// scanStruct keys on it to run promotion analysis — an anonymous
// struct that stays a RELATION instead (a Valuer whose GormDataType
// erased the DataType) reports reasonPlainStruct and promotes nothing.
const reasonEmbeddedStruct = "an embedded struct (only a driver.Valuer or a GormDataType of \"time\"/\"bytes\" stays a column)"

// classifyColumn is the syntactic mirror of GORM's DataType pipeline
// for a NAMED field (field.go, verified against v1.31.2: serializer /
// json → reflect kind → GormDataType overwrites UNCONDITIONALLY →
// snapshot → TYPE runs LAST). So: `gorm:"type:..."` always proves a
// column — it refills even a GormDataType-erased DataType (review
// round-8, latched); serializer proofs hold only until an exact
// GormDataType overwrites them; a Valuer's derived kind is overwritten
// the same way (the unwrap is guarded by the GormDataTypeInterface
// check, so both-implementers keep their GormDataType). An erased
// DataType re-enters the relation gate, which only struct shapes
// survive; a dynamic GormDataType without TYPE stays unknowable.
// Anonymous fields must go through classifyAnonymous instead — GORM's
// embed rule ignores these proofs for struct shapes and aborts on
// non-struct kinds outright.
func (sc *scanner) classifyColumn(typ ast.Expr, gormSettings map[string]string, imports map[string]string) (colVerdict, string) {
	hasType := gormSettings["TYPE"] != ""
	hasSerializer := gormSettings["SERIALIZER"] != "" || gormSettings["JSON"] != ""
	if name, isLocal := localBaseName(sc, typ); isLocal {
		valuer, _ := sc.resolveColumnMethod(name, "Value")
		dataTyper, dtState := sc.resolveColumnMethod(name, "GormDataType")
		if valuer == methodUnsure || dataTyper == methodUnsure {
			return colUnknown, ""
		}
		if dataTyper == methodYes {
			switch {
			case dtState.dynamic:
				if hasType {
					return colYes, "" // TYPE runs after the overwrite and fills the DataType either way
				}
				return colUnknown, "" // empty aborts, non-empty is a column — unprovable
			case dtState.literal == "":
				if hasType {
					return colYes, "" // latched: gorm:"type:..." rescues the erased DataType
				}
				// A serializer (or the Valuer-derived kind) does NOT
				// survive the GormDataType overwrite (round-8, latched).
				if sc.underlyingStruct(name, map[string]bool{}) != nil {
					return colNo, reasonPlainStruct // DataType-less struct: a relation
				}
				return colHardNo, "a type whose GormDataType returns the empty string (DataType-less at runtime)"
			default:
				return colYes, ""
			}
		}
		if hasType || hasSerializer {
			return colYes, "" // explicit column proof, any type goes
		}
		if valuer == methodYes {
			return colYes, ""
		}
		return sc.classifyShape(typ, imports, nil, map[string]bool{}, false)
	}
	if hasType || hasSerializer {
		return colYes, "" // cross-package: no visible GormDataType could overwrite the proof
	}
	return sc.classifyShape(typ, imports, nil, map[string]bool{}, false)
}

// localBaseName extracts the local named type behind pointers and
// generic instantiations, when there is one — methods declared on a
// generic type apply to every instantiation.
func localBaseName(sc *scanner, typ ast.Expr) (string, bool) {
	ident, ok := unwrapType(typ).(*ast.Ident)
	if !ok {
		return "", false
	}
	_, local := sc.types[ident.Name]
	return ident.Name, local
}

// binding is one generic type argument: the expression plus the import
// context it was written in (the instantiation site's file, or a
// previous binding's).
type binding struct {
	expr    ast.Expr
	imports map[string]string
}

// classifyShape resolves a type expression to its underlying shape,
// following local type chains with generic-argument substitution
// (review round-4: `type Bytes[T any] []T` instantiated as Bytes[byte]
// IS []byte). identityLost flips when a chain crosses a defined
// (non-alias) type: methods and named-type identity do not survive it,
// so a known cross-package column type reached that way is no longer
// provably a column — with one exception, time.Time, whose column-ness
// comes from reflect convertibility (verified against the runtime),
// which any underlying-preserving chain keeps.
func (sc *scanner) classifyShape(typ ast.Expr, imports map[string]string, env map[string]binding, seen map[string]bool, identityLost bool) (colVerdict, string) {
	for {
		if star, ok := typ.(*ast.StarExpr); ok {
			typ = star.X // GORM classifies through pointers
			continue
		}
		if paren, ok := typ.(*ast.ParenExpr); ok {
			typ = paren.X // grouping parens are pure syntax (review round-6)
			continue
		}
		break
	}
	switch t := typ.(type) {
	case *ast.Ident:
		if b, ok := env[t.Name]; ok {
			// A generic parameter: substitute the instantiation's
			// argument in its own import context.
			return sc.classifyShape(b.expr, b.imports, nil, seen, identityLost)
		}
		if info, local := sc.types[t.Name]; local {
			if seen[t.Name] {
				return colUnknown, "" // type cycle: broken code, don't hang
			}
			seen[t.Name] = true
			if len(info.typeParams) > 0 {
				return colUnknown, "" // bare generic name without arguments
			}
			return sc.classifyShape(info.decl, info.imports, nil, seen, identityLost || !info.isAlias)
		}
		if builtinScalars[t.Name] {
			return colYes, ""
		}
		if fatalScalars[t.Name] {
			return colHardNo, t.Name + " (GORM has no mapping for it and schema parsing aborts)"
		}
		return colUnknown, ""
	case *ast.IndexExpr, *ast.IndexListExpr:
		base, args := instantiation(t)
		base = ast.Unparen(base)
		if sel, ok := base.(*ast.SelectorExpr); ok {
			if !identityLost && selectorIsKnownColumn(sel, imports) {
				return colYes, "" // e.g. datatypes.JSONType[T]
			}
			return colUnknown, ""
		}
		ident, ok := base.(*ast.Ident)
		if !ok {
			return colUnknown, ""
		}
		info, local := sc.types[ident.Name]
		if !local || len(info.typeParams) != len(args) {
			return colUnknown, ""
		}
		if seen[ident.Name] {
			return colUnknown, ""
		}
		seen[ident.Name] = true
		newEnv := make(map[string]binding, len(args))
		for i, param := range info.typeParams {
			arg := args[i]
			argImports := imports
			if argIdent, ok := ast.Unparen(arg).(*ast.Ident); ok {
				if b, bound := env[argIdent.Name]; bound {
					arg, argImports = b.expr, b.imports
				}
			}
			newEnv[param] = binding{expr: arg, imports: argImports}
		}
		return sc.classifyShape(info.decl, info.imports, newEnv, seen, identityLost || !info.isAlias)
	case *ast.SelectorExpr:
		if isTimeTime(t, imports) {
			return colYes, "" // ConvertibleTo(time.Time) — survives defined types
		}
		if !identityLost && selectorIsKnownColumn(t, imports) {
			return colYes, ""
		}
		return colUnknown, ""
	case *ast.ArrayType:
		// GORM maps both slices AND fixed arrays of byte to a bytes
		// column (review round-3); the element must be IDENTICAL to the
		// predeclared byte type — reflect matches uint8 by type identity
		// (schema/field.go ByteReflectType), which generic substitution
		// and local alias chains preserve (review round-5) and a defined
		// byte type does not. Any other container is DataType-less: the
		// relation gate hands it to getOrParse, which strips containers
		// and pointers to the terminal type — then the relation switch
		// accepts only the Struct and Slice KINDS. A slice of structs
		// parses as a relation (skipped, DBName empty); a slice of
		// anything else aborts the whole model; a FIXED ARRAY aborts
		// whatever its element (review round-8, latched: "unsupported
		// data type ... on field"). Opaque terminals stay unknowable —
		// a relation or an abort, never a safe warning (round-8).
		if sc.isByteIdent(t.Elt, env) {
			return colYes, ""
		}
		elemKind := sc.shapeKindOf(t.Elt, imports, env, map[string]bool{}, true)
		if t.Len != nil {
			if elemKind == shapeOpaque {
				return colUnknown, "" // a cross-package alias of byte would be a bytes column
			}
			return colHardNo, "a fixed array of non-byte elements (the relation switch rejects the array kind)"
		}
		switch elemKind {
		case shapeStruct:
			return colNo, "a container of structs (a GORM relation, not a column)"
		case shapeScalar, shapeFatal:
			return colHardNo, "a container of non-struct elements (no schema shape without a serializer)"
		default:
			return colUnknown, ""
		}
	case *ast.StructType:
		return colNo, reasonPlainStruct
	case *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.InterfaceType:
		return colHardNo, "not a scalar type (no schema shape without a serializer)"
	default:
		return colUnknown, ""
	}
}

// isByteIdent reports whether an element expression denotes the
// predeclared byte/uint8 type IDENTICALLY: spelled directly, through
// the generic environment, through local alias chains — parenthesized
// or not — or through generic ALIAS instantiations (`type B[T any] =
// byte`, review round-6): the constructs reflect sees straight
// through. A defined byte type is a different type and stays rejected,
// as does a local declaration shadowing the predeclared names.
// Package-scope alias targets cannot see the instantiation
// environment, so following one resets it — a generic parameter and a
// package-level type may share a name.
func (sc *scanner) isByteIdent(elem ast.Expr, env map[string]binding) bool {
	seen := map[string]bool{}
	for {
		elem = ast.Unparen(elem)
		if base, args := instantiation(elem); args != nil {
			ident, ok := ast.Unparen(base).(*ast.Ident)
			if !ok {
				return false
			}
			info, local := sc.types[ident.Name]
			if !local || !info.isAlias || len(info.typeParams) != len(args) || seen[ident.Name] {
				return false // instantiating anything but a local generic alias mints a new identity
			}
			seen[ident.Name] = true
			newEnv := make(map[string]binding, len(args))
			for i, param := range info.typeParams {
				arg := args[i]
				if argIdent, ok := ast.Unparen(arg).(*ast.Ident); ok {
					if b, bound := env[argIdent.Name]; bound {
						arg = b.expr
					}
				}
				newEnv[param] = binding{expr: arg}
			}
			elem, env = info.decl, newEnv
			continue
		}
		ident, ok := elem.(*ast.Ident)
		if !ok {
			return false
		}
		if b, bound := env[ident.Name]; bound {
			elem, env = b.expr, nil // the argument was written at the instantiation site
			continue
		}
		if seen[ident.Name] {
			return false // alias cycle: broken code
		}
		seen[ident.Name] = true
		if info, local := sc.types[ident.Name]; local {
			if !info.isAlias || len(info.typeParams) > 0 {
				return false // a defined type never keeps the predeclared identity
			}
			elem, env = info.decl, nil // package-scope RHS: the instantiation env no longer applies
			continue
		}
		return ident.Name == "byte" || ident.Name == "uint8"
	}
}

// shapeKind buckets a type expression by the reflect kind GORM's two
// fatal switches key on (verified against gorm v1.31.2: field.go's
// EMBEDDED branch and the relation gate feeding getOrParse).
type shapeKind int

const (
	shapeOpaque    shapeKind = iota // not statically resolvable
	shapeStruct                     // struct kind: embeds expand, named fields parse as relations
	shapeScalar                     // bool/int*/uint*/float*/string kinds: plain columns everywhere
	shapeContainer                  // slice or array, reported only when containers are not stripped
	shapeFatal                      // map/chan/func/interface/uintptr/complex: no schema shape at all
)

// shapeKindOf resolves a type expression to its shapeKind, seeing
// through pointers, parens, local defined/alias chains and generic
// instantiations — the underlying KIND survives every one of them, so
// unlike column classification this walk follows defined generic types
// too. With stripContainers set it descends through slice/array
// elements the way schema.getOrParse does (the relation path); without
// it a container reports shapeContainer (the embed switch's view, where
// even a byte slice is fatal). Cross-package names resolve only for
// time.Time and the known-struct Valuers (sql.Null*, gorm.DeletedAt);
// gorm.io/datatypes is deliberately NOT here — its storage types mix
// kinds (JSON is a byte slice, JSONMap a map) — so it stays opaque.
// Package-scope targets reset the instantiation environment.
func (sc *scanner) shapeKindOf(typ ast.Expr, imports map[string]string, env map[string]binding, seen map[string]bool, stripContainers bool) shapeKind {
	for {
		if star, ok := typ.(*ast.StarExpr); ok {
			typ = star.X // reflect.Indirect / getOrParse dereference pointers
			continue
		}
		if paren, ok := typ.(*ast.ParenExpr); ok {
			typ = paren.X
			continue
		}
		break
	}
	switch t := typ.(type) {
	case *ast.StructType:
		return shapeStruct
	case *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.InterfaceType:
		return shapeFatal
	case *ast.ArrayType:
		if !stripContainers {
			return shapeContainer
		}
		return sc.shapeKindOf(t.Elt, imports, env, seen, true)
	case *ast.Ident:
		if b, ok := env[t.Name]; ok {
			return sc.shapeKindOf(b.expr, b.imports, nil, seen, stripContainers)
		}
		if info, local := sc.types[t.Name]; local {
			if seen[t.Name] || len(info.typeParams) > 0 {
				return shapeOpaque // cycle or bare generic name: broken code
			}
			seen[t.Name] = true
			return sc.shapeKindOf(info.decl, info.imports, nil, seen, stripContainers)
		}
		if builtinScalars[t.Name] {
			return shapeScalar
		}
		if fatalScalars[t.Name] {
			return shapeFatal
		}
		return shapeOpaque
	case *ast.IndexExpr, *ast.IndexListExpr:
		base, args := instantiation(t)
		ident, ok := ast.Unparen(base).(*ast.Ident)
		if !ok {
			return shapeOpaque
		}
		info, local := sc.types[ident.Name]
		if !local || len(info.typeParams) != len(args) || seen[ident.Name] {
			return shapeOpaque
		}
		seen[ident.Name] = true
		newEnv := make(map[string]binding, len(args))
		for i, param := range info.typeParams {
			arg, argImports := args[i], imports
			if argIdent, ok := ast.Unparen(arg).(*ast.Ident); ok {
				if b, bound := env[argIdent.Name]; bound {
					arg, argImports = b.expr, b.imports
				}
			}
			newEnv[param] = binding{expr: arg, imports: argImports}
		}
		return sc.shapeKindOf(info.decl, info.imports, newEnv, seen, stripContainers)
	case *ast.SelectorExpr:
		if isTimeTime(t, imports) {
			return shapeStruct
		}
		if x, ok := t.X.(*ast.Ident); ok {
			switch imports[x.Name] {
			case "database/sql":
				if strings.HasPrefix(t.Sel.Name, "Null") {
					return shapeStruct
				}
			case "gorm.io/gorm":
				if t.Sel.Name == "DeletedAt" {
					return shapeStruct
				}
			}
		}
		return shapeOpaque
	}
	return shapeOpaque
}

// instantiation splits a generic instantiation into its base expression
// and argument list.
func instantiation(typ ast.Expr) (ast.Expr, []ast.Expr) {
	switch t := typ.(type) {
	case *ast.IndexExpr:
		return t.X, []ast.Expr{t.Index}
	case *ast.IndexListExpr:
		return t.X, t.Indices
	}
	return typ, nil
}

// classifyAnonymous mirrors GORM's treatment of anonymous fields
// (gorm/schema/field.go, latched against v1.31.2): unless the value is
// a driver.Valuer or its pre-TYPE DataType snapshot is exactly "time"
// or "bytes", the field enters the EMBEDDED branch — a struct kind
// expands, scalar kinds fall through the switch and stay plain columns,
// and every other kind aborts schema parsing ("invalid embedded
// struct"). Serializer / json / type: proofs never prevent any of that
// (review round-7). TYPE still matters for what survives the branch: it
// runs AFTER the GormDataType overwrite, so it refills an erased
// DataType on scalar kinds and Valuer-exempt fields, which would
// otherwise abort in the relation gate (review round-8, latched). A
// GormDataType the scan cannot read stays undecidable — except when
// TYPE plus an exempt-or-fall-through kind makes the outcome a column
// on every branch.
func (sc *scanner) classifyAnonymous(typ ast.Expr, gormSettings map[string]string, imports map[string]string) (colVerdict, string) {
	if name, isLocal := localBaseName(sc, typ); isLocal {
		valuer, _ := sc.resolveColumnMethod(name, "Value")
		dataTyper, dtState := sc.resolveColumnMethod(name, "GormDataType")
		if valuer == methodUnsure || dataTyper == methodUnsure {
			return colUnknown, ""
		}
		hasType := gormSettings["TYPE"] != ""
		kind := sc.shapeKindOf(typ, imports, nil, map[string]bool{}, false)
		if dataTyper == methodYes {
			switch {
			case dtState.dynamic:
				if hasType && (valuer == methodYes || kind == shapeScalar) {
					return colYes, "" // exempt or falls through either way; TYPE fills the DataType
				}
				return colUnknown, "" // the snapshot decides the embed exemption — unprovable
			case dtState.literal == "time" || dtState.literal == "bytes":
				return colYes, "" // the two snapshot values the embed condition exempts
			case dtState.literal == "":
				if hasType && (valuer == methodYes || kind == shapeScalar) {
					return colYes, "" // latched: TYPE refills the erased DataType
				}
				if valuer == methodYes {
					// Exempt from the embed branch but DataType-less:
					// the relation gate takes it — a struct shape parses
					// as a relation, everything else aborts.
					switch kind {
					case shapeStruct:
						return colNo, reasonPlainStruct
					case shapeOpaque:
						return colUnknown, ""
					default:
						return colHardNo, "a type whose GormDataType returns the empty string (DataType-less at runtime)"
					}
				}
				if kind == shapeScalar {
					return colHardNo, "a type whose GormDataType returns the empty string (DataType-less at runtime)"
				}
				// Non-exempt snapshot: the embed branch's kind rules below.
			default:
				// A non-empty, non-exempt literal: exempt Valuers and
				// fall-through scalar kinds keep it as their DataType.
				if valuer == methodYes || kind == shapeScalar {
					return colYes, ""
				}
				// Non-exempt snapshot: the embed branch's kind rules below.
			}
		} else if valuer == methodYes {
			return colYes, "" // Valuer-exempt, DataType from the unwrapped value
		} else {
			// No column methods: bytes/time shapes are exempt snapshots
			// and scalar kinds fall through the switch — classifyShape's
			// colYes covers exactly those.
			if verdict, why := sc.classifyShape(typ, imports, nil, map[string]bool{}, false); verdict == colYes {
				return colYes, why
			}
		}
		switch kind {
		case shapeStruct:
			return colNo, reasonEmbeddedStruct
		case shapeContainer:
			return colHardNo, "a slice or array (GORM's embedded branch aborts on the kind: invalid embedded struct)"
		case shapeFatal:
			return colHardNo, "not a mappable kind (GORM's embedded branch aborts: invalid embedded struct)"
		case shapeScalar:
			return colYes, ""
		default:
			return colUnknown, ""
		}
	}
	// Cross-package or unresolvable: the known Valuers and time.Time are
	// exempt columns; everything else stays opaque — never provably a
	// struct, so never silently called an embed.
	return sc.classifyShape(typ, imports, nil, map[string]bool{}, false)
}

// carriesBase reports whether a local struct type transitively embeds
// an ENABLED chok base through exported anonymous embeds — GORM
// expands those, so the embedder is a runtime store model exactly like
// a direct db.Model embedder (review round-8). Embeds disabled by
// their gorm tag (the "-" family or fully closed permissions) do not
// expand and carry nothing.
func (sc *scanner) carriesBase(name string, seen map[string]bool) bool {
	if seen[name] {
		return false
	}
	seen[name] = true
	st, stImports := sc.underlyingStructWithImports(name, map[string]bool{})
	if st == nil {
		return false
	}
	dbImport := chokDBIdent(stImports)
	for _, f := range st.Fields.List {
		if len(f.Names) != 0 {
			continue
		}
		settings := parseGormSettings(fieldTag(f).Get("gorm"))
		if gormIgnored(settings) || permsAllClosed(settings) {
			continue
		}
		typ := unwrapType(f.Type)
		if isKnownBaseEmbed(typ, dbImport) {
			return true
		}
		ident, ok := typ.(*ast.Ident)
		if !ok || !ast.IsExported(ident.Name) {
			continue // unexported embedded field: GORM skips it
		}
		if sc.carriesBase(ident.Name, seen) {
			return true
		}
	}
	return false
}

// promotedFatal walks a local struct type the scanned model EXPANDS —
// an anonymous struct embed or a gorm:"embedded" wrapper target — and
// reports fatal shapes the promotion would carry into the model's
// schema (review round-8): the embedded SUB-parse skips the relation
// gate, but the promoted fields re-enter the OUTER one, and nested
// anonymous non-struct embeds abort the sub-parse itself. Returns the
// anonymous-class error (aborts wherever the type is parsed at all)
// and the named-class error (aborts the embedding model's direct
// parse); either may be nil. The walk applies the same per-field
// gates as scanStruct: gorm-ignored and fully closed fields are inert,
// tag proofs are honored, opaque shapes stay silent (the embed-level
// opaque warnings already cover them).
func (sc *scanner) promotedFatal(name string, seen map[string]bool) (anonErr, namedErr error) {
	if seen[name] {
		return nil, nil
	}
	seen[name] = true
	st, stImports := sc.underlyingStructWithImports(name, map[string]bool{})
	if st == nil {
		return nil, nil
	}
	dbImport := chokDBIdent(stImports)
	keep := func(ae, ne error) {
		if anonErr == nil {
			anonErr = ae
		}
		if namedErr == nil {
			namedErr = ne
		}
	}
	for _, f := range st.Fields.List {
		tag := fieldTag(f)
		settings := parseGormSettings(tag.Get("gorm"))
		if gormIgnored(settings) {
			continue
		}
		closed := permsAllClosed(settings)
		_, embedded := settings["EMBEDDED"]
		if len(f.Names) == 0 {
			typ := unwrapType(f.Type)
			if isKnownBaseEmbed(typ, dbImport) {
				continue
			}
			fieldName := embedFieldName(typ)
			if fieldName == "" || !ast.IsExported(fieldName) {
				continue
			}
			if embedded {
				switch sc.shapeKindOf(f.Type, stImports, nil, map[string]bool{}, false) {
				case shapeContainer, shapeFatal:
					keep(fmt.Errorf(
						"fieldgen: %s.%s: `gorm:\"embedded\"` on %s — not a struct, so GORM aborts schema parsing wherever %s is expanded; drop the embedded tag or restructure the field",
						name, fieldName, exprString(f.Type), name), nil)
				case shapeStruct:
					if ident, ok := typ.(*ast.Ident); ok {
						keep(sc.promotedFatal(ident.Name, seen))
					}
				}
				continue
			}
			verdict, why := sc.classifyAnonymous(f.Type, settings, stImports)
			switch verdict {
			case colHardNo:
				if !closed {
					keep(fmt.Errorf(
						"fieldgen: %s.%s: anonymous embed %s is %s — GORM aborts schema parsing wherever %s is parsed; name the field and prove a column with a serializer, exclude it (`gorm:\"-\"`), or close its permissions",
						name, fieldName, exprString(f.Type), why, name), nil)
				}
			case colNo:
				if !closed && why == reasonEmbeddedStruct {
					if ident, ok := typ.(*ast.Ident); ok {
						keep(sc.promotedFatal(ident.Name, seen))
					}
				}
			}
			continue
		}
		if embedded {
			exported := false
			for _, ident := range f.Names {
				exported = exported || ident.IsExported()
			}
			if !exported {
				continue
			}
			switch sc.shapeKindOf(f.Type, stImports, nil, map[string]bool{}, false) {
			case shapeContainer, shapeFatal:
				keep(fmt.Errorf(
					"fieldgen: %s.%s: `gorm:\"embedded\"` on %s — not a struct, so GORM aborts schema parsing wherever %s is expanded; drop the embedded tag or use a serializer instead",
					name, f.Names[0].Name, exprString(f.Type), name), nil)
			case shapeStruct:
				if ident, ok := unwrapType(f.Type).(*ast.Ident); ok {
					keep(sc.promotedFatal(ident.Name, seen))
				}
			}
			continue
		}
		for _, ident := range f.Names {
			if !ident.IsExported() {
				continue
			}
			verdict, why := sc.classifyColumn(f.Type, settings, stImports)
			if verdict == colHardNo && !closed {
				keep(nil, fmt.Errorf(
					"fieldgen: %s.%s: %s is %s — promoted into the embedding model's schema, where GORM aborts (unsupported data type); prove a column with a serializer or `gorm:\"type:...\"` tag, or exclude the field",
					name, ident.Name, exprString(f.Type), why))
			}
		}
	}
	return anonErr, namedErr
}

// methodVerdict is the three-valued outcome of selector resolution:
// unsure means the tree contains members the scan cannot see (an
// unverifiable cross-package embed shallow enough to shadow or create
// ambiguity), so neither "column" nor "relation" may be claimed.
type methodVerdict int

const (
	methodNo methodVerdict = iota
	methodYes
	methodUnsure
)

// embedEntry is one node of the promotion BFS: a local type name (or
// the known-valuer sentinel) at the current depth. multiples records
// that the node is reachable through two or more embedding paths at
// this same depth — Go then counts every member it contributes at
// least twice, ambiguous on its own (review round-5, the diamond).
type embedEntry struct {
	name      string
	multiples bool
}

// consolidateLevel merges duplicate entries of one BFS depth into a
// single entry with multiples set — same-depth path multiplicity is
// what Go's ambiguity rule counts, so it must survive deduplication
// (mirrors go/types lookupFieldOrMethod's consolidateMultiples).
// Entries stay in first-appearance order.
func consolidateLevel(entries []embedEntry) []embedEntry {
	if len(entries) < 2 {
		return entries
	}
	idx := make(map[string]int, len(entries))
	out := make([]embedEntry, 0, len(entries))
	for _, e := range entries {
		if i, ok := idx[e.name]; ok {
			out[i].multiples = true
			continue
		}
		idx[e.name] = len(out)
		out = append(out, e)
	}
	return out
}

// resolveColumnMethod resolves member ("Value" or "GormDataType")
// against a local type under Go's REAL selector rules (review round-4):
// the shallowest depth with any same-named field or method wins, and it
// must be exactly one exact-signature method — ambiguity at a depth, a
// wrong-signature method, or a mere field shadowing a deeper method all
// mean the interface is NOT implemented, which is exactly what GORM's
// type assertion sees. The walk is breadth-first with same-depth path
// multiplicity kept (review round-5): one type reached through two
// embedding paths at a depth contributes its members twice, and a
// deeper occurrence of an already-seen type loses to the shallower one.
func (sc *scanner) resolveColumnMethod(typeName, member string) (methodVerdict, methodState) {
	root := sc.aliasTerminal(typeName)
	if root.sel != nil {
		// The local name denotes a cross-package type outright — the
		// same rules as the selector written in place (review round-5).
		if selectorIsKnownValuer(root.sel, root.imports) {
			if member == "Value" {
				return methodYes, methodState{exact: true}
			}
			return methodNo, methodState{} // GormDataType unmodeled — Value already proves the column
		}
		if isTimeTime(root.sel, root.imports) {
			return methodNo, methodState{} // no such members; shape rules own time.Time
		}
		return methodUnsure, methodState{} // opaque cross-package identity
	}
	level := []embedEntry{{name: root.name}}
	seen := map[string]bool{}
	for len(level) > 0 {
		var (
			candidates int
			exactState methodState
			exactHits  int
			next       []embedEntry
			tainted    bool
		)
		for _, e := range level {
			weight := 1
			if e.multiples {
				weight = 2
			}
			if e.name == knownValuerNode {
				candidates += weight
				exactHits += weight
				exactState = methodState{exact: true}
				continue
			}
			if seen[e.name] {
				continue // reached at a shallower depth — that occurrence wins
			}
			seen[e.name] = true
			if st, ok := sc.methods[e.name][member]; ok {
				candidates += weight
				if st.exact {
					exactHits += weight
					exactState = st
				}
			}
			st, stImports := sc.underlyingStructWithImports(e.name, map[string]bool{})
			if st == nil {
				continue
			}
			for _, f := range st.Fields.List {
				if len(f.Names) > 0 {
					for _, ident := range f.Names {
						if ident.Name == member {
							candidates += weight // a field shadows any deeper method
						}
					}
					continue
				}
				embedName := embedFieldName(unwrapType(f.Type))
				if embedName == member {
					candidates += weight // the embedded field's own name shadows too
					continue
				}
				switch t := unwrapType(f.Type).(type) {
				case *ast.Ident:
					if _, local := sc.types[t.Name]; !local {
						// Undeclared identifier: broken code, invisible.
						tainted = true
						continue
					}
					term := sc.aliasTerminal(t.Name)
					if term.sel == nil {
						next = append(next, embedEntry{name: term.name, multiples: e.multiples})
						continue
					}
					// A local alias denoting a cross-package type embeds
					// that type itself (review round-5) — resolve it
					// exactly like the selector written directly.
					if selectorIsKnownValuer(term.sel, term.imports) {
						if member == "Value" {
							next = append(next, embedEntry{name: knownValuerNode, multiples: e.multiples})
						}
						continue
					}
					if isTimeTime(term.sel, term.imports) {
						continue
					}
					if isKnownBaseEmbed(term.sel, chokDBIdent(term.imports)) {
						continue // chok bases carry no column-proving members (named fields + base embeds only)
					}
					tainted = true
				case *ast.SelectorExpr:
					if selectorIsKnownValuer(t, stImports) {
						// Known Valuer: contributes an exact Value at
						// the NEXT depth and nothing deeper. Its
						// GormDataType surface is not modeled — the
						// Value promotion already proves the column.
						if member == "Value" {
							next = append(next, embedEntry{name: knownValuerNode, multiples: e.multiples})
						}
						continue
					}
					if isTimeTime(t, stImports) {
						continue // time.Time: no Value/GormDataType, no fields named so
					}
					if isKnownBaseEmbed(t, chokDBIdent(stImports)) {
						// chok bases (db.Model family, review round-8):
						// named fields and base embeds only — nothing to
						// promote or shadow, so an embedder like `type
						// Base struct{ db.Model }` stays resolvable.
						continue
					}
					tainted = true // opaque embed could shadow or clash
				default:
					tainted = true
				}
			}
		}
		switch {
		case candidates == 1 && exactHits == 1:
			return methodYes, exactState
		case candidates > 0:
			return methodNo, methodState{}
		case tainted:
			return methodUnsure, methodState{}
		}
		level = consolidateLevel(next)
	}
	return methodNo, methodState{}
}

// knownValuerNode is the sentinel level entry for a known cross-package
// driver.Valuer embed: one exact Value method, nothing else.
const knownValuerNode = "\x00known-valuer"

// aliasTerm is where a local alias chain lands: a local (non-alias)
// type name — or, when the chain leaves the package, the cross-package
// selector it denotes plus the imports of the file that declared the
// final alias (review round-5: `type Null = sql.NullString` embeds
// sql.NullString itself, so its promotions must apply).
type aliasTerm struct {
	name    string
	sel     *ast.SelectorExpr
	imports map[string]string
}

// aliasTerminal follows local alias chains (`type A = B`) to what they
// denote; methods declared with an alias receiver attach to the
// terminal local name.
func (sc *scanner) aliasTerminal(name string) aliasTerm {
	seen := map[string]bool{}
	for {
		if seen[name] {
			return aliasTerm{name: name}
		}
		seen[name] = true
		info, ok := sc.types[name]
		if !ok || !info.isAlias {
			return aliasTerm{name: name}
		}
		switch t := unwrapType(info.decl).(type) {
		case *ast.Ident:
			if _, local := sc.types[t.Name]; !local {
				return aliasTerm{name: name}
			}
			name = t.Name
		case *ast.SelectorExpr:
			return aliasTerm{name: name, sel: t, imports: info.imports}
		default:
			return aliasTerm{name: name}
		}
	}
}

// selectorIsKnownValuer reports cross-package types known to implement
// driver.Valuer (so embedding them promotes Value) — the known-column
// set minus time.Time, whose column-ness is convertibility, not a
// method.
func selectorIsKnownValuer(sel *ast.SelectorExpr, imports map[string]string) bool {
	x, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	switch imports[x.Name] {
	case "database/sql":
		return strings.HasPrefix(sel.Sel.Name, "Null")
	case "gorm.io/gorm":
		return sel.Sel.Name == "DeletedAt"
	case "gorm.io/datatypes":
		return datatypesStorage[sel.Sel.Name]
	}
	return false
}

func isTimeTime(sel *ast.SelectorExpr, imports map[string]string) bool {
	x, ok := sel.X.(*ast.Ident)
	return ok && imports[x.Name] == "time" && sel.Sel.Name == "Time"
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

// permsAllClosed reports the `->` / `<-` tag algebra closing every
// permission, mirroring gorm/schema/field.go's permission block —
// applied in ITS fixed order, not tag order: `->` always clears create
// and update and sets read from its value, then `<-` re-opens
// create/update and narrows by substring. With all three false the
// field never enters the anonymous-embed or relation branches and is
// inert at runtime (review round-7 finding 5) — note `->:false` ALONE
// closes everything. Value matching mirrors gorm exactly: `->`
// compares lowercased, `<-` matches "create"/"update" case-sensitively.
// The gorm:"-" family is gormIgnored's business, not modeled here.
func permsAllClosed(settings map[string]string) bool {
	creatable, updatable, readable := true, true, true
	if v, ok := settings["->"]; ok {
		creatable, updatable = false, false
		readable = strings.ToLower(v) != "false"
	}
	if v, ok := settings["<-"]; ok {
		creatable, updatable = true, true
		if v != "<-" {
			creatable = strings.Contains(v, "create")
			updatable = strings.Contains(v, "update")
		}
	}
	return !creatable && !updatable && !readable
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

// unwrapType strips pointers, grouping parens and generic
// instantiations down to the named type expression.
func unwrapType(expr ast.Expr) ast.Expr {
	for {
		switch t := expr.(type) {
		case *ast.StarExpr:
			expr = t.X
		case *ast.ParenExpr:
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

// resultTypes flattens a function's results, expanding grouped names
// ((a, b T) counts twice).
func resultTypes(ft *ast.FuncType) []ast.Expr {
	if ft.Results == nil {
		return nil
	}
	var types []ast.Expr
	for _, r := range ft.Results.List {
		n := len(r.Names)
		if n == 0 {
			n = 1
		}
		for range n {
			types = append(types, r.Type)
		}
	}
	return types
}

// resolveAliasedType follows local ALIAS chains only — the one Go
// construct that preserves type identity — so signature types written
// through aliases (review round-4: `type DV = driver.Value`) resolve to
// what they denote. Grouping parens are stripped (`type PV =
// (driver.Value)` denotes driver.Value itself) and a generic alias
// INSTANTIATION resolves through its right-hand side with the
// arguments substituted (`type VO[T any] = driver.Value`, both review
// round-6); package-scope targets reset the environment, which they
// cannot see. Defined types stop the chase: `type DV driver.Value` is
// a different type and must not match, and so is a defined generic
// type's instantiation. The denoted expression is otherwise kept WHOLE
// — an alias to *driver.Value or to a defined type's instantiation is
// that constructed type, not its base, and stripping the constructor
// minted false Valuers (review round-5).
func (sc *scanner) resolveAliasedType(typ ast.Expr, imports map[string]string, env map[string]binding, seen map[string]bool) (ast.Expr, map[string]string) {
	typ = ast.Unparen(typ)
	switch t := typ.(type) {
	case *ast.Ident:
		if b, bound := env[t.Name]; bound {
			return sc.resolveAliasedType(b.expr, b.imports, nil, seen)
		}
		info, local := sc.types[t.Name]
		if !local || !info.isAlias || len(info.typeParams) > 0 || seen[t.Name] {
			return typ, imports
		}
		seen[t.Name] = true
		return sc.resolveAliasedType(info.decl, info.imports, nil, seen)
	case *ast.IndexExpr, *ast.IndexListExpr:
		base, args := instantiation(t)
		ident, ok := ast.Unparen(base).(*ast.Ident)
		if !ok {
			return typ, imports
		}
		info, local := sc.types[ident.Name]
		if !local || !info.isAlias || len(info.typeParams) != len(args) || seen[ident.Name] {
			return typ, imports
		}
		seen[ident.Name] = true
		newEnv := make(map[string]binding, len(args))
		for i, param := range info.typeParams {
			arg, argImports := args[i], imports
			if argIdent, ok := ast.Unparen(arg).(*ast.Ident); ok {
				if b, bound := env[argIdent.Name]; bound {
					arg, argImports = b.expr, b.imports
				}
			}
			newEnv[param] = binding{expr: arg, imports: argImports}
		}
		return sc.resolveAliasedType(info.decl, info.imports, newEnv, seen)
	}
	return typ, imports
}

// isValuerMethod matches driver.Valuer exactly: `func (T) Value()
// (driver.Value, error)`, with signature types resolved through local
// aliases. The runtime type-asserts the interface, so a same-named
// method with any other signature must not count — review round-2
// caught Value() (int, error) still being a relation.
func (sc *scanner) isValuerMethod(d *ast.FuncDecl, imports map[string]string) bool {
	if d.Name.Name != "Value" || (d.Type.Params != nil && len(d.Type.Params.List) != 0) {
		return false
	}
	results := resultTypes(d.Type)
	if len(results) != 2 {
		return false
	}
	first, firstImports := sc.resolveAliasedType(results[0], imports, nil, map[string]bool{})
	sel, ok := first.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok || firstImports[x.Name] != "database/sql/driver" || sel.Sel.Name != "Value" {
		return false
	}
	second, _ := sc.resolveAliasedType(results[1], imports, nil, map[string]bool{})
	err, ok := second.(*ast.Ident)
	return ok && err.Name == "error" && !sc.shadowedPredeclared("error")
}

// isGormDataTypeMethod matches GORM's GormDataTypeInterface:
// `func (T) GormDataType() string`, with the result type resolved
// through local aliases. A type implementing it gets a DataType (hence
// a DBName) from GORM even when it is a struct.
func (sc *scanner) isGormDataTypeMethod(d *ast.FuncDecl, imports map[string]string) bool {
	if d.Name.Name != "GormDataType" || (d.Type.Params != nil && len(d.Type.Params.List) != 0) {
		return false
	}
	results := resultTypes(d.Type)
	if len(results) != 1 {
		return false
	}
	first, _ := sc.resolveAliasedType(results[0], imports, nil, map[string]bool{})
	ident, ok := first.(*ast.Ident)
	return ok && ident.Name == "string" && !sc.shadowedPredeclared("string")
}

// shadowedPredeclared reports a package-level DEFINED type shadowing a
// predeclared identifier — pathological, but then the bare name no
// longer denotes the predeclared type. (An alias to it was already
// chased by resolveAliasedType.)
func (sc *scanner) shadowedPredeclared(name string) bool {
	info, ok := sc.types[name]
	return ok && !info.isAlias
}

// methodStringLiteral extracts the returned string when the method body
// is a single `return "literal"` — the only form the scan can prove.
// Everything else reports dynamic.
func methodStringLiteral(d *ast.FuncDecl) (literal string, dynamic bool) {
	if d.Body == nil || len(d.Body.List) != 1 {
		return "", true
	}
	ret, ok := d.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return "", true
	}
	lit, ok := ret.Results[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", true
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", true
	}
	return s, false
}

// typeParamNames lists a generic declaration's parameter names in
// order.
func typeParamNames(s *ast.TypeSpec) []string {
	if s.TypeParams == nil {
		return nil
	}
	var names []string
	for _, f := range s.TypeParams.List {
		for _, n := range f.Names {
			names = append(names, n.Name)
		}
	}
	return names
}

// embedFieldName is the field name Go gives an embedded type: its base
// name.
func embedFieldName(typ ast.Expr) string {
	switch t := typ.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	}
	return ""
}

func exprString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return exprString(e.X) + "." + e.Sel.Name
	case *ast.StarExpr:
		return "*" + exprString(e.X)
	case *ast.ParenExpr:
		return "(" + exprString(e.X) + ")"
	case *ast.IndexExpr:
		return exprString(e.X) + "[...]"
	case *ast.IndexListExpr:
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
