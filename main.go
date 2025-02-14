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

	printed := make(map[string]bool)
	results := make(map[string]*printOutput)

	for _, sym := range symbols {
		if printed[sym] {
			continue
		}
		printed[sym] = true

		pkgPath, receiverType, isPtr, funcOrTypeName, err := parseSymbol(sym)
		if err != nil {
			log.Printf("skip symbol %q: %v\n", sym, err)
			continue
		}

		pkgs, err := loadPackages(absRoot, pkgPath)
		if err != nil {
			log.Printf("failed to load package %q for symbol %q: %v\n", pkgPath, sym, err)
			continue
		}

		found := false

		for _, pkg := range pkgs {
			decl, filePath, fset := findFuncDecl(pkg, receiverType, isPtr, funcOrTypeName)
			if decl == nil {
				continue
			}
			src, err := extractNodeSource(fset, filePath, decl)
			if err != nil {
				log.Printf("failed to extract source of %q: %v\n", sym, err)
				continue
			}
			if _, ok := results[pkgPath]; !ok {
				results[pkgPath] = &printOutput{
					pkgName:     pkg.Name,
					pkgPath:     pkgPath,
					definitions: []string{},
				}
			}
			results[pkgPath].definitions = append(results[pkgPath].definitions, src)
			found = true
			break
		}

		if found {
			continue
		}

		for _, pkg := range pkgs {
			node, filePath, fset := findTypeSpec(pkg, funcOrTypeName)
			if node == nil {
				continue
			}
			src, err := extractNodeSource(fset, filePath, node)
			if err != nil {
				log.Printf("failed to extract type source of %q: %v\n", sym, err)
				continue
			}
			if _, ok := results[pkgPath]; !ok {
				results[pkgPath] = &printOutput{
					pkgName:     pkg.Name,
					pkgPath:     pkgPath,
					definitions: []string{},
				}
			}
			results[pkgPath].definitions = append(results[pkgPath].definitions, src)
			found = true
			break
		}

		if !found {
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
		if *formatFlag == "markdown" {
			fmt.Printf("### %s\n\n", out.pkgPath)
			fmt.Println("```go")
			fmt.Printf("package %s\n\n", out.pkgName)

			defLens := len(out.definitions)
			for i, snippet := range out.definitions {
				fmt.Println(snippet)
				if i != defLens-1 {
					fmt.Println()
				}
			}
			fmt.Println("```")
			fmt.Println()
		} else {
			fmt.Printf("Package: %s (package %s)\n", out.pkgPath, out.pkgName)
			fmt.Println("--------------------------------------------------")
			fmt.Printf("package %s\n\n", out.pkgName)
			defLens := len(out.definitions)
			for i, snippet := range out.definitions {
				fmt.Println(snippet)
				if i != defLens-1 {
					fmt.Println()
				}
			}
			fmt.Println("--------------------------------------------------")
			fmt.Println()
		}
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

func findFuncDecl(pkg *packages.Package, receiverType string, isPtr bool, funcName string) (decl *ast.FuncDecl, filePath string, fset *token.FileSet) {
	for i, fAST := range pkg.Syntax {
		fileName := pkg.CompiledGoFiles[i]
		fset = pkg.Fset

		for _, d := range fAST.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fd.Name == nil || fd.Name.Name != funcName {
				continue
			}
			if receiverType == "" {
				if fd.Recv == nil {
					return fd, fileName, fset
				}
				continue
			}
			if fd.Recv == nil || len(fd.Recv.List) == 0 {
				continue
			}
			recvExpr := fd.Recv.List[0].Type
			if matchReceiverType(recvExpr, receiverType, isPtr) {
				return fd, fileName, fset
			}
		}
	}
	return nil, "", nil
}

func matchReceiverType(expr ast.Expr, typeName string, isPtr bool) bool {
	switch e := expr.(type) {
	case *ast.StarExpr:
		if !isPtr {
			return false
		}
		if id, ok := e.X.(*ast.Ident); ok {
			return id.Name == typeName
		}
		if sel, ok := e.X.(*ast.SelectorExpr); ok {
			return sel.Sel.Name == typeName
		}
		return false
	case *ast.Ident:
		if isPtr {
			return false
		}
		return e.Name == typeName
	case *ast.SelectorExpr:
		if isPtr {
			return false
		}
		return e.Sel.Name == typeName
	case *ast.ParenExpr:
		return matchReceiverType(e.X, typeName, isPtr)
	}
	return false
}

func findTypeSpec(pkg *packages.Package, typeName string) (node ast.Node, filePath string, fset *token.FileSet) {
	for i, fAST := range pkg.Syntax {
		fileName := pkg.CompiledGoFiles[i]
		fset = pkg.Fset

		for _, d := range fAST.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, sp := range gd.Specs {
				ts, ok := sp.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if ts.Name != nil && ts.Name.Name == typeName {
					return gd, fileName, fset
				}
			}
		}
	}
	return nil, "", nil
}

func extractNodeSource(fset *token.FileSet, filePath string, node ast.Node) (string, error) {
	srcBytes, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("cannot read file: %w", err)
	}
	start := fset.Position(node.Pos()).Offset
	end := fset.Position(node.End()).Offset
	if start >= len(srcBytes) || end > len(srcBytes) {
		return "", fmt.Errorf("invalid positions: start=%d end=%d len=%d", start, end, len(srcBytes))
	}
	return string(srcBytes[start:end]), nil
}
