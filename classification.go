package main

import (
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/manishrjain/keys"
	yaml "gopkg.in/yaml.v2"
)

const dateIsoLedger = "2006/1/2"

const SamePayeeAction = 999991.0
const SameAmountAction = 999990.0

func (p *parser) categorizeTxn(t *Txn, idx, total int) float64 {
	clear()
	printSummary(*t, idx, total)
	fmt.Println()
	if len(t.Desc) > descLength {
		color.New(color.BgWhite, color.FgBlack).Printf("%6s %s ", "[DESC]", t.Desc) // descLength used in Printf.
		fmt.Println()
	}
	{
		cat := getCategory(*t)
		if len(cat) > catLength {
			color.New(color.BgGreen, color.FgBlack).Printf("%s", cat)
			fmt.Println()
		}
	}
	fmt.Println()

	hits := p.topHits(t.Desc)
	var ks keys.Shortcuts
	setDefaultMappings(&ks)
	for _, hit := range hits {
		ks.AutoAssign(string(hit), "default")
	}

	res := p.printAndGetResult(ks, t)
	switch res {
	case math.MaxFloat32:
		clear()
		printSummary(*t, idx, total)
		return p.fuzzyAndGetResult(&existingAccounts, t)
	case SamePayeeAction, SameAmountAction:
		execAction(res, t)
		clear()
		printSummary(*t, idx, total)
		return p.categorizeTxn(t, idx, total)
	}
	return res
}

func (p *parser) classifyTxn(t *Txn) {
	if !t.Done {
		hits := p.topHits(t.Desc)
			t.From = string(hits[0])
	}
}

// Assign account to transaction, smartly according to the sign of the amount
func assignAccount(t *Txn, accountName string) {
	t.From = accountName
}

func (p *parser) fuzzyAndGetResult(existingAccounts *AccountSet, t *Txn) float64 {
	accountName := fuzzySelectAccount(t, existingAccounts)
	assignAccount(t, accountName)
	if len(t.To) > 0 && len(t.From) > 0 {
		p.writeToDB(*t)
		t.Done = true
		return 1.0
	}
	return 0
}

func (p *parser) printAndGetResult(ks keys.Shortcuts, t *Txn) float64 {
	label := "default"

	var repeat bool
	var category []string
LOOP:
	if len(category) > 0 {
		fmt.Println()
		color.New(color.BgWhite, color.FgBlack).Printf("Selected [%s]", strings.Join(category, ":")) // descLength used in Printf.
		fmt.Println()
	}

	ks.Print(label, false)
	r := make([]byte, 1)
	_, err := os.Stdin.Read(r)
	checkf(err, "Unable to read stdin")
	ch := rune(r[0])
	if ch == rune(10) && len(t.To) > 0 && len(t.From) > 0 {
		p.writeToDB(*t)
		t.Done = true
		if repeat {
			return 0.0
		}
		return 1.0
	}

	if opt, has := ks.MapsTo(ch, label); has {
		switch opt {
		case ".back":
			return -1.0
		case ".skip":
			return 1.1
		case ".quit":
			return 999999.0
		case ".show same amount":
			return SameAmountAction
		case ".show same payee":
			return SamePayeeAction
		case ".show all":
			return math.MaxFloat32
		}

		category = append(category, opt)
		assignAccount(t, strings.Join(category, ":"))
		label = opt
		if ks.HasLabel(label) {
			repeat = true
			goto LOOP
		}
	}
	return 0
}

var lettersOnly = regexp.MustCompile("[^a-zA-Z]+")

func (p *parser) showAndCategorizeTxns(rtxns []Txn) {
	txns := rtxns
	for {
		for i := 0; i < len(txns); i++ {
			t := &txns[i]
			p.classifyTxn(t)
			printSummary(*t, i, len(txns))
		}
		fmt.Println()

		fmt.Printf("Found %d transactions. Review (Y/n/q)? ", len(txns))
		b := make([]byte, 1)
		_, _ = os.Stdin.Read(b)
		if b[0] == 'n' || b[0] == 'q' {
			return
		}

		applyToSimilarTxns := func(from int) int {
			t := txns[from]
			src := lettersOnly.ReplaceAllString(t.Desc, "")
			for i := from + 1; i < len(txns); i++ {
				dst := &txns[i]
				if src != lettersOnly.ReplaceAllString(dst.Desc, "") {
					return i
				}
				if math.Signbit(t.Cur) != math.Signbit(dst.Cur) {
					return i
				}

				dst.From = t.From
				dst.Done = true
			}
			return len(txns)
		}

		for i := 0; i < len(txns) && i >= 0; {
			t := &txns[i]
			res := p.categorizeTxn(t, i, len(txns))
			if res == 1.0 {
				upto := applyToSimilarTxns(i)
				if upto == i+1 {
					// Did not find anything.
					i += int(res)
					continue
				}
				clear()
				printSummary(txns[i], i, len(txns))
				for j := i + 1; j < upto; j++ {
					printSummary(txns[j], j, len(txns))
					p.writeToDB(txns[j])
				}
				fmt.Println()
				fmt.Println("The above txns were similar to the last categorized txns, " +
					"and were categorized accordingly. Can be changed by skipping back and forth.")
				r := make([]byte, 1)
				_, _ = os.Stdin.Read(r)
				i = upto
			} else {
				i += int(res)
			}
		}
	}
}

// This function would use a rules.yaml file in this format:
// Expenses:Travel:
//   - regexp-for-description
//   - ^LYFT\ +\*RIDE
// Expenses:Food:
//   - ^STARBUCKS
// ...
// If this file is present, txns would be auto-categorized, if their description
// matches the regular expressions provided.
func (p *parser) categorizeByRules(txns []Txn) []Txn {
	fpath := path.Join(*configDir, "rules.yaml")
	data, err := ioutil.ReadFile(fpath)
	if err != nil {
		return txns
	}

	rules := make(map[string][]string)
	checkf(yaml.Unmarshal(data, &rules), "Unable to parse auto.yaml confict at %s", fpath)

	matchesCategory := func(t Txn) string {
		for category, patterns := range rules {
			for _, pattern := range patterns {
				match, err := regexp.Match(pattern, []byte(t.Desc))
				checkf(err, "Unable to parse regexp")
				if match {
					return category
				}
			}
		}
		return ""
	}

	unmatched := txns[:0]
	var count int
	for _, t := range txns {
		if cat := matchesCategory(t); len(cat) > 0 {
				t.From = cat
			count++
			printSummary(t, count, count)
			p.writeToDB(t)
		} else {
			unmatched = append(unmatched, t)
		}
	}
	fmt.Printf("\t%d txns have been categorized based on rules.\n\n", len(txns)-len(unmatched))
	return unmatched
}

func sanitize(a string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r
		}
		if r >= '0' && r <= '9' {
			return r
		}
		switch r {
		case '*':
			fallthrough
		case ':':
			fallthrough
		case '/':
			fallthrough
		case '.':
			fallthrough
		case '-':
			return r
		default:
			return -1
		}
	}, a)
}

func (p *parser) removeDuplicates(txns []Txn) []Txn {
	if len(txns) == 0 {
		return txns
	}

	sort.Sort(byTime(p.txns))
	sort.Sort(byTime(txns))

	prev := p.txns
	first := txns[0].Date.Add(-24 * time.Hour)
	for i, t := range p.txns {
		if t.Date.After(first) {
			prev = p.txns[i:]
			break
		}
	}

	allowed := time.Duration(*dupWithin) * time.Hour
	within := func(a, b time.Time) bool {
		dur := a.Sub(b)
		return math.Abs(float64(dur)) <= float64(allowed)
	}

	final := txns[:0]
	for _, t := range txns {
		var found bool
		tdesc := sanitize(t.Desc)
		for _, pr := range prev {
			if pr.Date.After(t.Date.Add(allowed)) {
				break
			}
			pdesc := sanitize(pr.Desc)
			if tdesc == pdesc && within(pr.Date, t.Date) && math.Abs(pr.Cur) == math.Abs(t.Cur) {
				printSummary(t, 0, 0)
				found = true
				break
			}
		}
		if !found {
			final = append(final, t)
		}
	}
	fmt.Printf("\t%d duplicates found and ignored.\n\n", len(txns)-len(final))
	return final
}

// TODO Move to UI file?

// dateBeforeAfter returns two strings in a ledger friendly format, one
// corresponding to a date before and another to a date after. This spans a bit
// more than two months
func dateBeforeAfter(date time.Time) (before, after string) {
	before = date.AddDate(0, -1, -2).Format(dateIsoLedger)
	after = date.AddDate(0, 1, 2).Format(dateIsoLedger)
	return before, after
}

func execAction(action float64, t *Txn) {
	clear()
	switch action {
	case SamePayeeAction:
		showWithPayee(t)
	case SameAmountAction:
		showWithAmount(t)
	default:
		panic(fmt.Sprintln("unsupported action", action))
	}

	fmt.Println("Press any key")
	r := make([]byte, 1)
	_, err := os.Stdin.Read(r)
	checkf(err, "Unable to read stdin")
}

func showWithPayee(t *Txn) {
	before, after := dateBeforeAfter(t.Date)
	ledgerCmd := []string{"ledger", "r", "-f", *journal,
		"-b", before, "-e", after,
		"@" + t.Desc,
	}
	fmt.Println(strings.Join(ledgerCmd, " "))
	fmt.Println(runCommandRaw(ledgerCmd[0], ledgerCmd[1:]...))
}

func showWithAmount(t *Txn) {
	before, after := dateBeforeAfter(t.Date)
	ledgerCmd := []string{"ledger", "r", "-f", *journal,
		"-b", before, "-e", after,
		"--display", fmt.Sprintf("quantity(amount) == %f", t.Cur),
	}
	fmt.Println(strings.Join(ledgerCmd, " "))
	fmt.Println(runCommandRaw(ledgerCmd[0], ledgerCmd[1:]...))
}
