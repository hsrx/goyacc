// Copyright 2014 The goyacc Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This source code uses portions of code previously published in the Go tool
// yacc[0] program, the respective license can be found in the LICENSE-GO-YACC
// file.

// Goyacc is a version of yacc generating Go parsers.
//
// Usage:
//
//	goyacc [options] [input]
//
//	options and (defaults)
//		-c			report state closures
//		-ex			explain how were conflicts resolved
//		-l			disable line directives (false); for compatibility only - ignored
//		-la			report all lookahead sets
//		-o outputFile		parser output ("y.go")
//		-p prefix		name prefix to use in generated code ("yy")
//		-v reportFile		create grammar report ("y.output")
//		-xe examplesFile	generate error messages by examples ("")
//
// If no non flag arguments are given, goyacc reads standard input.
//
// The generated parser is reentrant and mostly backwards compatible with
// parsers generated by go tool yacc[0]. yyParse expects to be given an
// argument that conforms to the following interface:
//
//	type yyLexer interface {
//		Lex(lval *yySymType) int
//		Error(e string)
//	}
//
// Lex should return the token identifier, and place other token information in
// lval (which replaces the usual yylval). Error is equivalent to yyerror in
// the original yacc.
//
// Code inside the parser may refer to the variable yylex, which holds the
// yyLexer passed to Parse.
//
// Multiple grammars compiled into a single program should be placed in
// distinct packages. If that is impossible, the "-p prefix" flag to yacc sets
// the prefix, by default yy, that begins the names of symbols, including
// types, the parser, and the lexer, generated and referenced by yacc's
// generated code. Setting it to distinct values allows multiple grammars to be
// placed in a single package.
//
// Extensions wrt go tool yacc
//
// - goyacc implements ideas from "Generating LR Syntax Error Messages from
// Examples"[1]. Use the -xe flag to pass a name of the example file. For more
// details about the example format please see [2].
//
// - The grammar report includes example token sequences leading to the
// particular state.
//
// - Minor changes/improvements of parser debugging.
//
// Links
//
// Referenced from elsewhere:
//
//  [0]: http://golang.org/cmd/yacc/
//  [1]: http://people.via.ecp.fr/~stilgar/doc/compilo/parser/Generating%20LR%20Syntax%20Error%20Messages.pdf
//  [2]: http://godoc.org/github.com/cznic/y#hdr-Error_Examples
package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/scanner"
	"go/token"
	"io"
	"io/ioutil"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/cznic/mathutil"
	yscanner "github.com/cznic/scanner/yacc"
	"github.com/cznic/sortutil"
	"github.com/cznic/strutil"
	"github.com/cznic/y"
)

var (
	//oNoDefault = flag.Bool("nodefault", false, "disable generating $default actions")
	oClosures = flag.Bool("c", false, "report state closures")
	oLA       = flag.Bool("la", false, "report all lookahead sets")
	oNoLines  = flag.Bool("l", false, "disable line directives (for compatibility ony - ignored)")
	oOut      = flag.String("o", "y.go", "parser output")
	oPref     = flag.String("p", "yy", "name prefix to use in generated code")
	oReport   = flag.String("v", "y.output", "create grammar report")
	oResolved = flag.Bool("ex", false, "explain how were conflicts resolved")
	oXErrors  = flag.String("xe", "", "generate eXtra errors from examples source file")
)

func main() {
	log.SetFlags(log.Ldate | log.Lmicroseconds)

	defer func() {
		if e := recover(); e != nil {
			log.Fatal(e)
		}
	}()

	flag.Parse()
	var in string
	switch flag.NArg() {
	case 0:
		in = os.Stdin.Name()
	case 1:
		in = flag.Arg(0)
	default:
		log.Fatal("expected at most one non flag argument")
	}

	if err := main1(in); err != nil {
		switch x := err.(type) {
		case scanner.ErrorList:
			for _, v := range x {
				fmt.Fprintf(os.Stderr, "%v\n", v)
			}
			os.Exit(1)
		default:
			log.Fatal(err)
		}
	}
}

type symUsed struct {
	sym  *y.Symbol
	used int
}

type symsUsed []symUsed

func (s symsUsed) Len() int      { return len(s) }
func (s symsUsed) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

func (s symsUsed) Less(i, j int) bool {
	if s[i].used > s[j].used {
		return true
	}

	if s[i].used < s[j].used {
		return false
	}

	return strings.ToLower(s[i].sym.Name) < strings.ToLower(s[j].sym.Name)
}

func main1(in string) error {
	var out io.Writer
	if nm := *oOut; nm != "" {
		f, err := os.Create(nm)
		if err != nil {
			return err
		}

		defer f.Close()
		w := bufio.NewWriter(f)
		defer w.Flush()
		out = w
	}

	var rep io.Writer
	if nm := *oReport; nm != "" {
		f, err := os.Create(nm)
		if err != nil {
			return err
		}

		defer f.Close()
		w := bufio.NewWriter(f)
		defer w.Flush()
		rep = w
	}

	var xerrors []byte
	if nm := *oXErrors; nm != "" {
		b, err := ioutil.ReadFile(nm)
		if err != nil {
			return err
		}

		xerrors = b
	}

	p, err := y.ProcessFile(token.NewFileSet(), in, &y.Options{
		//NoDefault:   *oNoDefault,
		AllowConflicts: true,
		Closures:       *oClosures,
		LA:             *oLA,
		Report:         rep,
		Resolved:       *oResolved,
		XErrorsName:    *oXErrors,
		XErrorsSrc:     xerrors,
	})
	if err != nil {
		return err
	}

	msu := make(map[*y.Symbol]int, len(p.Syms)) // sym -> usage
	for nm, sym := range p.Syms {
		if nm == "" || nm == "ε" || nm == "$accept" || nm == "#" {
			continue
		}

		msu[sym] = 0
	}
	var minArg, maxArg int
	for _, state := range p.Table {
		for _, act := range state {
			msu[act.Sym]++
			k, arg := act.Kind()
			if k == 'a' {
				continue
			}

			if k == 'r' {
				arg = -arg
			}
			minArg, maxArg = mathutil.Min(minArg, arg), mathutil.Max(maxArg, arg)
		}
	}
	su := make(symsUsed, 0, len(msu))
	for sym, used := range msu {
		su = append(su, symUsed{sym, used})
	}
	sort.Sort(su)

	// ----------------------------------------------------------- Prologue
	f := strutil.IndentFormatter(out, "\t")
	f.Format("%s", injectImport(p.Prologue))
	f.Format(`
type %[1]sSymType %i%s%u

type %[1]sXError struct {
	state, xsym int
}
`, *oPref, p.UnionSrc)

	// ---------------------------------------------------------- Constants
	nsyms := map[string]*y.Symbol{}
	a := make([]string, 0, len(msu))
	maxTokName := 0
	for sym := range msu {
		nm := sym.Name
		if nm == "$default" || sym.IsTerminal && nm[0] != '\'' && sym.Value > 0 {
			maxTokName = mathutil.Max(maxTokName, len(nm))
			a = append(a, nm)
		}
		nsyms[nm] = sym
	}
	sort.Strings(a)
	f.Format("\nconst (%i\n")
	for _, v := range a {
		nm := v
		switch nm {
		case "error":
			continue
		case "$default":
			nm = *oPref + "Default"
		}
		f.Format("%s%s= %d\n", nm, strings.Repeat(" ", maxTokName-len(nm)+1), nsyms[v].Value)
	}
	minArg-- // eg: [-13, 42], minArg -14 maps -13 to 1 so zero cell values -> empty.
	f.Format("\n%sTabOfs = %d\n", *oPref, minArg)
	f.Format("%u)")

	// ---------------------------------------------------------- Variables
	f.Format("\n\nvar (%i\n")

	// Lex translation table
	f.Format("%sXLAT = map[int]int{%i\n", *oPref)
	xlat := make(map[int]int, len(su))
	var endSym, errSym int
	for i, v := range su {
		if v.sym.Name == "$end" {
			endSym = i
		}
		if v.sym.Name == "error" {
			errSym = i
		}
		xlat[v.sym.Value] = i
		f.Format("%6d: %3d, // %s (%dx)\n", v.sym.Value, i, v.sym.Name, msu[v.sym])
	}
	f.Format("%u}\n")

	// Symbol names
	f.Format("\n%sSymNames = []string{%i\n", *oPref)
	for _, v := range su {
		f.Format("%q,\n", v.sym.Name)
	}
	f.Format("%u}\n")

	// Reduction table
	f.Format("\n%sReductions = map[int]struct{xsym, components int}{%i\n", *oPref)
	for r, rule := range p.Rules {
		f.Format("%d: {%d, %d},\n", r, xlat[rule.Sym.Value], len(rule.Components))
	}
	f.Format("%u}\n")

	// XError table
	f.Format("\n%[1]sXErrors = map[%[1]sXError]string{%i\n", *oPref)
	for _, xerr := range p.XErrors {
		state := xerr.Stack[len(xerr.Stack)-1]
		xsym := -1
		if xerr.Lookahead != nil {
			xsym = xlat[xerr.Lookahead.Value]
		}
		f.Format("%[1]sXError{%d, %d}: %q,\n", *oPref, state, xsym, xerr.Msg)
	}
	f.Format("%u}\n\n")

	// Parse table
	tbits := 32
	switch n := mathutil.BitLen(maxArg - minArg + 1); {
	case n < 8:
		tbits = 8
	case n < 16:
		tbits = 16
	}
	f.Format("%sParseTab = [%d][]uint%d{%i\n", *oPref, len(p.Table), tbits)
	nCells := 0
	var tabRow sortutil.Uint64Slice
	for si, state := range p.Table {
		tabRow = tabRow[:0]
		max := 0
		for _, act := range state {
			sym := act.Sym
			xsym, ok := xlat[sym.Value]
			if !ok {
				panic("internal error 001")
			}

			max = mathutil.Max(max, xsym)
			kind, arg := act.Kind()
			switch kind {
			case 'a':
				arg = 0
			case 'r':
				arg *= -1
			}
			tabRow = append(tabRow, uint64(xsym)<<32|uint64(arg-minArg))
		}
		nCells += max
		tabRow.Sort()
		col := -1
		if si%5 == 0 {
			f.Format("// %d\n", si)
		}
		f.Format("{")
		for i, v := range tabRow {
			xsym := int(uint32(v >> 32))
			arg := int(uint32(v))
			if col+1 != xsym {
				f.Format("%d: ", xsym)
			}
			switch {
			case i == len(tabRow)-1:
				f.Format("%d", arg)
			default:
				f.Format("%d, ", arg)
			}
			col = xsym
		}
		f.Format("},\n")
	}
	f.Format("%u}\n")
	fmt.Fprintf(os.Stderr, "Parse table has %d cells (of %d), x %d bits == %d bytes\n", nCells, len(p.Table)*len(msu), tbits, nCells*tbits/8)
	if n := p.ConflictsSR; n != 0 {
		fmt.Fprintf(os.Stderr, "conflicts: %d shift/reduce\n", n)
	}
	if n := p.ConflictsRR; n != 0 {
		fmt.Fprintf(os.Stderr, "conflicts: %d reduce/reduce\n", n)
	}

	f.Format(`%u)

var %[1]sDebug = 0

type %[1]sLexer interface {
	Lex(lval *%[1]sSymType) int
	Error(s string)
}

const %[1]sEOF = %d

func %[1]sSymName(c int) (s string) {
	if c >= 0 && c < len(%[1]sSymNames) {
		return %[1]sSymNames[c]
	}

	return __yyfmt__.Sprintf("%%q", c)
}

func %[1]slex1(lex %[1]sLexer, lval *%[1]sSymType) (n int) {
	n = lex.Lex(lval)
	if n <= 0 {
		n = -1
	}
	n = %[1]sXLAT[n]
	if %[1]sDebug >= 3 {
			__yyfmt__.Printf("\nlex %%s(%%d)\n\n", %[1]sSymName(n), n)
	}
	return n
}

func %[1]sParse(yylex %[1]sLexer) int {
	const yyError = %[3]d
	var lval, rval %[1]sSymType
	errState := 0
	yyerrok := func() { 
		if %[1]sDebug >= 2 {
			__yyfmt__.Printf("\tyyerrok()\n\n")
		}
		errState = 0
	}
	_ = yyerrok
	stack := []%[1]sSymType{{}}
	lookahead := -1
next:
	sp := len(stack)-1
	state := stack[sp].yys
	if lookahead < 0 {
		lookahead = %[1]slex1(yylex, &lval)
	}
	if %[1]sDebug >= 4 {
		var a []int
		for _, v := range stack {
			a = append(a, v.yys)
		}
		__yyfmt__.Printf("state %%d, lookahead %%v, states stack %%v\n", state, %[1]sSymName(lookahead), a)
	}
	if %[1]sDebug >= 6 {
		__yyfmt__.Printf("\tlval %%+v\n", lval)
		__yyfmt__.Printf("\trval %%+v\n", rval)
	}
	if %[1]sDebug >= 7 {
		__yyfmt__.Printf("\tfull stack %%+v\n", stack)
	}
	row := %[1]sParseTab[state]
	arg := 0
	if lookahead < len(row) {
		arg = int(row[lookahead])
		if arg != 0 {
			arg += %[1]sTabOfs
		}
	}
	switch {
	case arg > 0: // shift
		lval.yys = arg
		stack = append(stack, lval)
		lval = %[1]sSymType{}
		if errState > 0 {
			errState--
		}
		lookahead = -1
		if %[1]sDebug >= 4 {
			__yyfmt__.Printf("\tshift, and goto state %%d\n", arg)
		}
		goto next
	case arg < 0: // reduce
	case state == 1: // accept
		return 0
	default: // error
		switch errState {
		case 0:
			if %[1]sDebug >= 1 {
				__yyfmt__.Printf("\tstate %%d, unexpected lookahead %%s\n", state, %[1]sSymName(lookahead))
			}
			k := %[1]sXError{state, lookahead}
			if %[1]sDebug >= 5 {
				__yyfmt__.Printf("\terror recovery looking for xerror key {state %%d, lookahead %%s}\n", state, %[1]sSymName(lookahead))
			}
			msg, ok := %[1]sXErrors[k]
			if !ok {
				k.xsym = -1
				if %[1]sDebug >= 5 {
					__yyfmt__.Printf("\terror recovery looking for xerror key {state %%d, lookahead <nil>}\n", state)
				}
				msg, ok = %[1]sXErrors[k]
			}
			if !ok {
				msg = "syntax error"
			}
			yylex.Error(msg)
			fallthrough
		case 1, 2:
			errState = 3
			for sp != 0 {
				row := %[1]sParseTab[state]
				if yyError < len(row) {
					arg = int(row[yyError])+%[1]sTabOfs
					if arg != 0 { // hit
						if %[1]sDebug >= 2 {
							__yyfmt__.Printf("\terror recovery found error shift in state %%d\n\n", state)
						}
						lval.yys = arg
						stack = append(stack, lval)
						lval = %[1]sSymType{}
						goto next
					}
				}

				stack = stack[:sp]
				sp--
				state = stack[sp].yys
				if %[1]sDebug >= 2 {
					__yyfmt__.Printf("\terror recovery pops state %%d\n", state)
				}
			}

			if %[1]sDebug >= 2 {
				__yyfmt__.Printf("\terror recovery failed\n\n")
			}
			return 1
		case 3:
			if %[1]sDebug >= 2 {
				__yyfmt__.Printf("\terror recovery discards %%s\n", %[1]sSymName(lookahead))
			}
			if lookahead == %[1]sEOF {
				return 1
			}

			lookahead = -1
			goto next
		}
		return 1
	}

	r := -arg
	x0 := %[1]sReductions[r]
	x, n := x0.xsym, x0.components
	rval.yys = int(%[1]sParseTab[stack[sp-n].yys][x])+%[1]sTabOfs
	if %[1]sDebug >= 4 {
		__yyfmt__.Printf("\treduce rule %%d (%%s), and goto state %%d\n", r, %[1]sSymName(x), rval.yys)
	}
	switch r {%i
`,
		*oPref, endSym, errSym)
	for r, rule := range p.Rules {
		components := rule.Components
		typ := rule.Sym.Type
		max := len(components)
		synth := false
		if p := rule.Parent; p != nil {
			max = rule.MaxParentDlr
			components = p.Components
			synth = true
		}
		action := rule.Action
		if len(action) == 0 && typ == "" {
			continue
		}

		f.Format("case %d: ", r)
		if len(action) == 0 && !synth {
			f.Format("%i{\nrval.%s = stack[sp].%s%u\n}\n", typ, p.Syms[components[0]].Type)
			continue
		}

		for _, part := range action {
			num := part.Num
			f.Format("%s", part.Src)
			switch part.Tok {
			case yscanner.DLR_DLR:
				f.Format("rval.%s", typ)
			case yscanner.DLR_NUM:
				f.Format("stack[sp-%d].%s", max-num, p.Syms[components[num-1]].Type)
			case yscanner.DLR_TAG_DLR:
				f.Format("rval.%s", part.Tag)
			case yscanner.DLR_TAG_NUM:
				f.Format("stack[sp-%d].%s", num, part.Tag)
			}
		}
		f.Format("\n")
	}
	f.Format(`%u
	}

	stack = append(stack[:sp-n+1], rval)
	goto next
}

%[2]s
`, *oPref, p.Tail)
	_ = oNoLines //TODO Ignored for now
	return nil
}

func injectImport(src string) string {
	const inj = `

import __yyfmt__ "fmt"
`
	fset := token.NewFileSet()
	file := fset.AddFile("", -1, len(src))
	var s scanner.Scanner
	s.Init(
		file,
		[]byte(src),
		nil,
		scanner.ScanComments,
	)
	for {
		switch _, tok, _ := s.Scan(); tok {
		case token.EOF:
			return inj + src
		case token.PACKAGE:
			s.Scan() // ident
			pos, _, _ := s.Scan()
			ofs := file.Offset(pos)
			return src[:ofs] + inj + src[ofs:]
		}
	}
}
