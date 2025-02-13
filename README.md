# symbolprint

A command-line tool for extracting Go function, method, and type definitions.  
It reads symbol names from standard input and prints the corresponding source snippets.

## Installation  

```
go install github.com/kis9a/symbolprint@latest
```

## Usage

*Symbol Formats*
  - `package/path.FuncName`  
  - `package/path.TypeName`  
  - `(package/path.TypeName).MethodName` or `(*package/path.TypeName).MethodName`  

*Output formats*
  - `-format=plain`
  - `-format=markdown`
  
## Example

```
$ symbolprint -format=markdown . <<<"$(
  cat <<EOF
github.com/example/project/pkg.Add
(*github.com/example/project/pkg.Calc).Add
EOF
)"

Package: github.com/example/project/pkg.Add (package pkg)
--------------------------------------------------
package pkg

func Add(a, b int) int {
    return a + b
}

func (c *Calc) Add() error {...}
--------------------------------------------------
```

Use with [calldigraph](https://github.com/kis9a/calldigraph) and [digraph](https://golang.org/x/tools/cmd/digraph).

```
$ calldigraph -symbol 'github.com/example/api/usecase.(*Usecase).Do' \
  | digraph nodes \
  | grep -v 'exclude_pattern' \
  | symbolprint -format markdown .

...
```
