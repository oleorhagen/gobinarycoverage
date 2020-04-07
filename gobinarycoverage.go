// Copyright 2020 Northern.tech AS
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//        http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.
//
// Usage:
//
//    instrumentmain mainPackage
//
//        Enables coverage of all the files in the mainPackage listed,
//        and outputs a dynamically generated new main file on stdout,
//        which encorporates all the variables from the files that
//        are to be analyzed for their coverage.
//
//     Note:
//        The files in the packages listed will be changed locally.
//
//
// Environment variables:
//
//  - COVERAGE_FILENAME: The suffix given to the coverage file created
//  - COVERAGE_FILEPATH: The directory in which to put the coverage file

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/template"

	// Parse Go source code
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
)

var usageString string = `
Usage:

   gobinarycoverage package [package]...

       Enables coverage of all the files in the packages listed,
       and outputs a dynamically generated new main file on stdout,
       which encorporates all the variables from the files that
       are to be analyzed for their coverage.

    Note:
       The files in the packages listed will be changed locally.


Environment variables:

     - COVERAGE_FILENAME: The suffix given to the coverage file created
     - COVERAGE_FILEPATH: The directory in which to put the coverage file
`

// The structure generated by go tool cover
// var GoCover = struct {
// 	Count     [117]uint32
// 	Pos       [3 * 117]uint32
// 	NumStmt   [117]uint16
// }

// coverInfo holds a map to the names of the cover variables
type coverInfo struct {
	Package string
	Vars    map[string]*CoverVar
}

// CoverVar is a simple set collecting the GoCover variable name along with its
// source file
type CoverVar struct {
	File string
	Var  string
}

// ReplaceFilecontents replaces the dst file contents with the contents of src.
func replaceFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	if err != nil {
		return err
	}
	return out.Close()
}

// Package is for use with `go list -json`
type Package struct {
	Dir        string // Directory containing the source files
	GoFiles    []string
	ImportPath string

	Imports   []string          // imports used by this package
	ImportMap map[string]string // map from source import to ImportPath (identity entries are omitted)

	Deps []string
}

func listPackagesImported(packageName string) (packages []string, imports []string, importsMap map[string]string, dir string, err error) {
	cmd := exec.Command(
		"go", "list",
		"-json",
		packageName,
	)
	buf := bytes.NewBuffer(nil)
	cmd.Stdout = buf
	if err = cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "`go list -json %s failed. Error: %s\n", packageName, err.Error())
		return nil, nil, nil, "", err
	}
	// The go list command returns a json byte array parse this into the
	// appropriate structure, from which we can extract all the Go files present
	// in the package
	p := &Package{}
	if err = json.Unmarshal(buf.Bytes(), p); err != nil {
		fmt.Fprintf(os.Stderr, "`go list -json %s failed. Error: %s\n", packageName, err.Error())
		return nil, nil, nil, "", err
	}
	// Filter all the non-local dependencies, and vendored packages
	// i.e., remove all local libraries, and vendored packages
	var coverPackages []string
	for _, pName := range p.Deps {
		if strings.Contains(pName, p.ImportPath) && !strings.Contains(pName, "/vendor/") {
			coverPackages = append(coverPackages, pName)
		}
	}
	return coverPackages, p.Imports, p.ImportMap, p.Dir, nil
}

// getFilesInPackage employs `go list 'packageName'` to extract all the files in
// the given package
func getFilesInPackage(packageName string) (p *Package, err error) {
	cmd := exec.Command(
		"go", "list",
		"-json",
		packageName,
	)
	buf := bytes.NewBuffer(nil)
	cmd.Stdout = buf
	if err = cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "`go list -json %s failed. Error: %s\n", packageName, err.Error())
		return nil, err
	}
	// The go list command returns a json byte array parse this into the
	// appropriate structure, from which we can extract all the Go files present
	// in the package
	p = &Package{}
	if err = json.Unmarshal(buf.Bytes(), p); err != nil {
		fmt.Fprintf(os.Stderr, "`go list -json %s failed. Error: %s\n", packageName, err.Error())
		return nil, err
	}
	return p, nil
}

// instrumentFileInPackage runs `go tool cover` on all the go source files in
// the named package
func instrumentFilesInPackage(packageName string) (cInfo *coverInfo, err error) {
	tdir, err := ioutil.TempDir("", "instrumentFiles")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tdir)

	// Store the package name along with the GoCover variable names
	cInfo = &coverInfo{Package: packageName, Vars: make(map[string]*CoverVar)}

	p, err := getFilesInPackage(packageName)
	if err != nil {
		return nil, err
	}

	// covstructName is a function which generates the name of the coverage
	// struct, with an integer suffix in order to differentiate amongst them
	// globally.
	counter := 1
	covStructName := func(fileName string) string {
		s := "GoCover" + strconv.Itoa(counter)
		counter += 1
		// Add the name of the variable to the coverInfo struct
		cInfo.Vars[fileName] = &CoverVar{File: fileName, Var: s}
		return s
	}

	for _, name := range p.GoFiles {
		tname := tdir + name
		fname := p.Dir + "/" + name        // name with the full path prefixed
		rname := p.ImportPath + "/" + name // name with the relative import path for coverage output
		// 1) Generate the instrumented source code using the `go tool cover`
		// functionality. The instrumented file is created in the temporary dir,
		// tdir.
		cmd := exec.Command(
			"go", "tool", "cover",
			"-mode=set",
			"-var", covStructName(rname),
			"-o", tname,
			fname)
		buf := bytes.NewBuffer(nil)
		cmd.Stderr = buf
		if err = cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "go tool cover %s, failed. Error: %s\nOutput: %s\n",
				fname, err.Error(), buf.String())
			return nil, err
		}
		// 2) Replace the original source code file, with the instrumented one
		// generated above.
		if err = replaceFileContents(tname, fname); err != nil {
			return nil, err
		}
	}
	return cInfo, nil
}

func parseMainGoFile(fset *token.FileSet, filePath string) (*ast.File, error) {
	// fset := token.NewFileSet() // positions are relative to fset
	// Parse src but stop after processing the imports.
	f, err := parser.ParseFile(fset, filePath, nil, 0) // Parse all the things
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to parse the file: %s. Error: %s\n", filePath, err.Error())
		return nil, err
	}
	return f, nil
}

// mergeASTTrees takes two AST trees, and merges them (if possible) into a
// single unified ast, and returns it. The merging is naive, and does no fancy
// heurestics for resolving conflicts. Conflicts will have to be solved by a
// human.
func mergeASTTrees(fset *token.FileSet, t1 *ast.File, t2 *ast.File) (*bytes.Buffer, error) {

	// Merge the imports from both files
	ast.Inspect(t1, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.GenDecl:
			if x.Tok == token.IMPORT {
				// Walk the second tree until we find the import statements
				ast.Inspect(t2, func(n ast.Node) bool {
					switch y := n.(type) {
					case *ast.GenDecl:
						if y.Tok == token.IMPORT {
							// Add all the children to the t1 tree's import statement
							x.Specs = append(x.Specs, y.Specs...)
							return false // Stop the iteration
						}
					}
					return true
				})
				return false
			}
		}
		return true

	})

	// Merge the declarations from t2 into t1
	for _, decl := range t2.Decls {
		if d, isDecl := decl.(*ast.GenDecl); isDecl {
			if d.Tok == token.IMPORT {
				continue
			}
		}
		t1.Decls = append(t1.Decls, decl)
	}

	// Print the modified AST to buf.
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, t1); err != nil {
		panic(err)
	}

	return &buf, nil
}

// Cover is passed in to the main.go template, and expands all the needed
// GoCover variables, and imports all the packages we are covering.
type Cover struct {
	CoverInfo []*coverInfo
	Imports   []string          // The packages the main file imports (generated by go list on the package provided no the CLI)
	ImportMap map[string]string // Resolves coverage paths TODO -- how to use this?
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "%s\n", usageString)
		os.Exit(1)
	}
	// Collect all coverage meta-data in the Cover struct. This is needed for the
	// template generation of main later on.
	cov := Cover{}
	//
	// Get all the packages imported by main
	//
	packageList, imports, importMap, dir, err := listPackagesImported(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to list the packages imported by: %s. Error: %s\n", os.Args[1], err.Error())
		os.Exit(1)
	}
	cov.Imports = imports
	cov.ImportMap = importMap
	//
	// Parse the main.go file
	//
	fset := token.NewFileSet() // positions are relative to fset
	originalMainAST, err := parseMainGoFile(fset, dir+"/main.go")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to parse main.go\nError: %s\n", err.Error())
		os.Exit(1)
	}
	//
	// Instrument the source files in the given package with coverage functionality
	//
	for _, pname := range packageList {
		cInfo, err := instrumentFilesInPackage(pname)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to instrument the files in package: %s\nError: %s\n",
				os.Args[1], err.Error())
			os.Exit(1)
		}
		cov.CoverInfo = append(cov.CoverInfo, cInfo)
	}
	// TODO - Merge the syntax trees of the generated template, and the main.go file parsed
	generatedMainAST, err := generateMainFromTemplate(fset, &cov)
	//
	// merge the two AST's
	//
	buf, err := mergeASTTrees(fset, generatedMainAST, originalMainAST)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to merge the generated main file with the main file of the package: Error: %s\n", err.Error())
		os.Exit(1)
	}
	//
	// Replace the main file with the new merged contents
	//
	f, err := os.OpenFile(dir + "/main.go", os.O_WRONLY, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open the main.go file. Error: %s\n", err.Error())
		os.Exit(1)
	}
	_, err = io.Copy(f, buf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to replace the contents of main.go. Error: %s\n", err.Error())
		os.Exit(1)
	}
	os.Exit(0)
}

func generateMainFromTemplate(fset *token.FileSet, cover *Cover) (*ast.File, error) {
	tmpl, err := template.New("Main").Parse(testmainTmplStr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Failed to parse the main.go template. Error: %s\n", err.Error())
		return nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, cover); err != nil {
		fmt.Fprintln(os.Stderr, "Failed to execute the main.go template. Error: %s\n", err.Error())
		return nil, err
	}
	// Parse the template file generated into an AST
	f, err := parser.ParseFile(fset, "", buf.String(), 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to parse the generated main file. Error: %s\n", err.Error())
		return nil, err
	}
	return f, nil
}

var testmainTmplStr string = `
package main

import (
  "fmt"
  "io/ioutil"
	"testing"

// Import all the GoCover variables from the packages which are coverage instrumented
  {{range $i, $ci := .CoverInfo}}
    _cover{{$i}} {{$ci.Package | printf "%q"}}
  {{end}}

)

var (
	coverCounters = make(map[string][]uint32)
	coverBlocks = make(map[string][]testing.CoverBlock)
)

func init() {
  // Register the addresses of all the GoCover variables from all the packages
  // to be covered
	{{range $i, $p := .CoverInfo}}
	  {{range $file, $cover := $p.Vars}}
	 coverRegisterFile({{printf "%q" $cover.File}}, _cover{{$i}}.{{$cover.Var}}.Count[:], _cover{{$i}}.{{$cover.Var}}.Pos[:], _cover{{$i}}.{{$cover.Var}}.NumStmt[:])
	  {{end}}
	{{end}}

}

func coverRegisterFile(fileName string, counter []uint32, pos []uint32, numStmts []uint16) {
	if 3*len(counter) != len(pos) || len(counter) != len(numStmts) {
		panic("coverage: mismatched sizes")
	}
	if coverCounters[fileName] != nil {
		// Already registered.
		return
	}
	coverCounters[fileName] = counter
	block := make([]testing.CoverBlock, len(counter))
	for i := range counter {
		block[i] = testing.CoverBlock{
			Line0: pos[3*i+0],
			Col0: uint16(pos[3*i+2]),
			Line1: pos[3*i+1],
			Col1: uint16(pos[3*i+2]>>16),
			Stmts: numStmts[i],
		}
	}
	coverBlocks[fileName] = block
}

func coverReport() {

  reportFile, err := ioutil.TempFile(os.Getenv("COVERAGE_FILEPATH"), "coverage" + os.Getenv("COVERAGE_FILENAME") + ".out")
  if err != nil {
    return
  }

	var active, total int64
	var count uint32
	for name, counts := range coverCounters {
		blocks := coverBlocks[name]
		for i := range counts {
			stmts := int64(blocks[i].Stmts)
			total += stmts
			if counts[i] > 0 {
				active += stmts
			}
			fmt.Fprintf(reportFile, "%s:%d.%d,%d.%d %d %d\n", name,
				blocks[i].Line0, blocks[i].Col0,
				blocks[i].Line1, blocks[i].Col1,
				stmts,
				count)
		}
	}
	if total == 0 {
		fmt.Fprintln(reportFile, "coverage: [no statements]")
		return
	}
	fmt.Fprintf(reportFile, "coverage: %.1f%% of statements%s\n", 100*float64(active)/float64(total), "github.com/mendersoftware/mender")
  fmt.Fprintf(os.Stderr, "Wrote coverage to the file: %s\n", reportFile.Name())

}
`
