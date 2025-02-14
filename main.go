package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

type printOutput struct {
	pkgName     string
	pkgPath     string
	definitions []string
}

type functionKey struct {
	funcName     string
	receiverType string
	isPtr        bool
}

type packageIndex struct {
	pkg          *packages.Package
	fileContents map[string][]byte
	funcDecls    map[functionKey]*ast.FuncDecl
	typeSpecs    map[string]*ast.GenDecl
	fset         *token.FileSet
}

func main() {
	formatFlag := flag.String("format", "plain", "output format: plain or markdown")
	flag.Parse()
	args := flag.Args()

	if len(args) < 1 {
		log.Fatalf("Usage: %s <module-root>\n", os.Args[0])
	}
	rootDir := args[0]

	symbols, err := readSymbolsFromStdin()
	if err != nil {
		log.Fatalf("failed to read symbols: %v", err)
	}
	if len(symbols) == 0 {
		log.Println("No symbols found in input")
		return
	}

	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		log.Fatalf("failed to get absolute module root path: %v", err)
	}

	symbolsByPkg := make(map[string][]string)
	printed := make(map[string]bool)

	for _, sym := range symbols {
		if printed[sym] {
			continue
		}
		printed[sym] = true

		pkgPath, _, _, _, parseErr := parseSymbol(sym)
		if parseErr != nil {
			log.Printf("skip symbol %q: %v\n", sym, parseErr)
			continue
		}
		symbolsByPkg[pkgPath] = append(symbolsByPkg[pkgPath], sym)
	}

	results := make(map[string]*printOutput)
	for pkgPath, syms := range symbolsByPkg {
		pkgs, err := loadPackages(absRoot, pkgPath)
		if err != nil {
			log.Printf("failed to load package %q: %v\n", pkgPath, err)
			continue
		}
		pkg := pkgs[0]

		idx := buildPackageIndex(pkg)

		for _, sym := range syms {
			pkgPath, receiverType, isPtr, funcOrTypeName, err := parseSymbol(sym)
			if err != nil {
				log.Printf("skip symbol %q: %v\n", sym, err)
				continue
			}

			if _, ok := results[pkgPath]; !ok {
				results[pkgPath] = &printOutput{
					pkgName:     pkg.Name,
					pkgPath:     pkgPath,
					definitions: []string{},
				}
			}

			fnKey := functionKey{
				funcName:     funcOrTypeName,
				receiverType: receiverType,
				isPtr:        isPtr,
			}
			if decl, ok := idx.funcDecls[fnKey]; ok {
				src, err := idx.extractNodeSource(decl, decl.Pos(), decl.End())
				if err != nil {
					log.Printf("failed to extract source of %q: %v\n", sym, err)
					continue
				}
				results[pkgPath].definitions = append(results[pkgPath].definitions, src)
				continue
			}

			if genDecl, ok := idx.typeSpecs[funcOrTypeName]; ok {
				src, err := idx.extractNodeSource(genDecl, genDecl.Pos(), genDecl.End())
				if err != nil {
					log.Printf("failed to extract type source of %q: %v\n", sym, err)
					continue
				}
				results[pkgPath].definitions = append(results[pkgPath].definitions, src)
				continue
			}

			log.Printf("No matching function or type declaration found for symbol %q\n", sym)
		}
	}

	pkgPaths := make([]string, 0, len(results))
	for p := range results {
		pkgPaths = append(pkgPaths, p)
	}
	sort.Strings(pkgPaths)

	for _, pkgKey := range pkgPaths {
		out := results[pkgKey]
		switch *formatFlag {
		case "markdown":
			fmt.Printf("### %s\n\n", out.pkgPath)
			fmt.Println("```go")
			fmt.Printf("package %s\n\n", out.pkgName)
			for i, snippet := range out.definitions {
				fmt.Println(snippet)
				if i != len(out.definitions)-1 {
					fmt.Println()
				}
			}
			fmt.Println("```")
			fmt.Println()

		default:
			fmt.Printf("Package: %s (package %s)\n", out.pkgPath, out.pkgName)
			fmt.Println("--------------------------------------------------")
			fmt.Printf("package %s\n\n", out.pkgName)
			for i, snippet := range out.definitions {
				fmt.Println(snippet)
				if i != len(out.definitions)-1 {
					fmt.Println()
				}
			}
			fmt.Println("--------------------------------------------------")
			fmt.Println()
		}
	}
}

func buildPackageIndex(pkg *packages.Package) *packageIndex {
	idx := &packageIndex{
		pkg:          pkg,
		fset:         pkg.Fset,
		fileContents: make(map[string][]byte),
		funcDecls:    make(map[functionKey]*ast.FuncDecl),
		typeSpecs:    make(map[string]*ast.GenDecl),
	}

	for _, fAST := range pkg.Syntax {
		for _, d := range fAST.Decls {
			switch decl := d.(type) {
			case *ast.FuncDecl:
				name := decl.Name.Name
				var recvType string
				var isPtr bool
				if decl.Recv != nil && len(decl.Recv.List) > 0 {
					recvExpr := decl.Recv.List[0].Type
					rt, ptr := receiverTypeString(recvExpr)
					recvType = rt
					isPtr = ptr
				}
				key := functionKey{
					funcName:     name,
					receiverType: recvType,
					isPtr:        isPtr,
				}
				idx.funcDecls[key] = decl

			case *ast.GenDecl:
				if decl.Tok == token.TYPE {
					for _, sp := range decl.Specs {
						ts, ok := sp.(*ast.TypeSpec)
						if !ok {
							continue
						}
						typeName := ts.Name.Name
						idx.typeSpecs[typeName] = decl
					}
				}
			}
		}
	}
	return idx
}

func (idx *packageIndex) extractNodeSource(node ast.Node, startPos, endPos token.Pos) (string, error) {
	filePos := idx.fset.Position(startPos)
	fileEnd := idx.fset.Position(endPos)
	filePath := filePos.Filename

	content, err := idx.getFileContent(filePath)
	if err != nil {
		return "", err
	}

	startOffset := filePos.Offset
	endOffset := fileEnd.Offset
	if startOffset >= len(content) || endOffset > len(content) {
		return "", fmt.Errorf("invalid positions: start=%d end=%d len=%d", startOffset, endOffset, len(content))
	}
	return string(content[startOffset:endOffset]), nil
}

func (idx *packageIndex) getFileContent(filePath string) ([]byte, error) {
	if b, ok := idx.fileContents[filePath]; ok {
		return b, nil
	}
	b, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("cannot read file '%s': %w", filePath, err)
	}
	idx.fileContents[filePath] = b
	return b, nil
}

func receiverTypeString(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.StarExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			return id.Name, true
		}
		if sel, ok := e.X.(*ast.SelectorExpr); ok {
			return sel.Sel.Name, true
		}
		return "", true
	case *ast.Ident:
		return e.Name, false
	case *ast.SelectorExpr:
		return e.Sel.Name, false
	case *ast.ParenExpr:
		return receiverTypeString(e.X)
	default:
		return "", false
	}
}

func readSymbolsFromStdin() ([]string, error) {
	var symbols []string
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.Contains(line, "->") {
			parts := strings.Split(line, "->")
			if len(parts) == 2 {
				left := strings.TrimSpace(parts[0])
				right := strings.TrimSpace(parts[1])
				if left != "" {
					symbols = append(symbols, left)
				}
				if right != "" {
					symbols = append(symbols, right)
				}
			}
		} else {
			symbols = append(symbols, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return symbols, nil
}

func parseSymbol(symbol string) (pkgPath, receiverType string, isPtr bool, funcOrTypeName string, err error) {
	methodRegex := regexp.MustCompile(`^\(\*?([^)]+)\)\.([^.]+)$`)
	funcRegex := regexp.MustCompile(`^(.+)\.([^.]+)$`)

	switch {
	case methodRegex.MatchString(symbol):
		m := methodRegex.FindStringSubmatch(symbol)
		if len(m) != 3 {
			err = fmt.Errorf("invalid method symbol: %s", symbol)
			return
		}
		raw := m[1]
		funcOrTypeName = m[2]
		isPtr = strings.HasPrefix(symbol, "(*")

		lastDot := strings.LastIndex(raw, ".")
		if lastDot == -1 {
			err = fmt.Errorf("cannot split pkgPath and type from %q", raw)
			return
		}
		pkgPath = raw[:lastDot]
		receiverType = raw[lastDot+1:]

	case funcRegex.MatchString(symbol):
		m := funcRegex.FindStringSubmatch(symbol)
		if len(m) != 3 {
			err = fmt.Errorf("invalid symbol: %s", symbol)
			return
		}
		pkgPath = m[1]
		funcOrTypeName = m[2]
		isPtr = false
		receiverType = ""

	default:
		err = fmt.Errorf("symbol format not recognized: %s", symbol)
	}
	return
}

func loadPackages(dir, importPath string) ([]*packages.Package, error) {
	cfg := &packages.Config{
		Dir:   dir,
		Mode:  packages.NeedName | packages.NeedTypes | packages.NeedSyntax | packages.NeedCompiledGoFiles,
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, importPath)
	if err != nil {
		return nil, fmt.Errorf("packages.Load error: %w", err)
	}
	for _, p := range pkgs {
		if len(p.Errors) > 0 {
			return nil, fmt.Errorf("package load error: %v", p.Errors)
		}
	}
	if len(pkgs) == 0 {
		return nil, errors.New("no packages found")
	}
	return pkgs, nil
}
