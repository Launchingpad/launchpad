package importer

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
)

// Print writes the report as plain tables: counts only, never ticket text
// or a user's name or email.
func (rep *Report) Print(w io.Writer) error {
	mode := "IMPORT"
	if rep.DryRun {
		mode = "DRY RUN (rolled back, nothing was written)"
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "%s\t\t\t\t\t\t\n", mode)
	fmt.Fprintln(tw, "entity\texport\texcluded\tcreate\tupdate\tskip\t")
	for _, e := range Entities {
		c := rep.Counts[e]
		if c == nil {
			continue
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t\n", e, c.Export, c.Excluded, c.Create, c.Update, c.Skip)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if len(rep.Exclusions) > 0 {
		type key struct{ ent, reason string }
		by := map[key]int{}
		for _, x := range rep.Exclusions {
			by[key{x.Entity, x.Reason}]++
		}
		keys := make([]key, 0, len(by))
		for k := range by {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].ent != keys[j].ent {
				return keys[i].ent < keys[j].ent
			}
			return keys[i].reason < keys[j].reason
		})
		fmt.Fprintln(w, "\nexcluded:")
		for _, k := range keys {
			fmt.Fprintf(w, "  %-20s %5d  %s\n", k.ent, by[k], k.reason)
		}
	}
	fmt.Fprintf(w, "\nlabels merged by name: %d, relations merged (same pair): %d\n", rep.MergedLabels, rep.MergedRelations)
	for _, k := range sortedKeys(rep.MissingCycleNumbers) {
		nums := make([]string, 0, len(rep.MissingCycleNumbers[k]))
		for _, n := range rep.MissingCycleNumbers[k] {
			nums = append(nums, fmt.Sprint(n))
		}
		fmt.Fprintf(w, "cycle numbers missing in Linear for %s: %s\n", k, strings.Join(nums, ", "))
	}
	for _, k := range sortedKeys(rep.Cadence) {
		fmt.Fprintf(w, "cadence %s: %s\n", k, rep.Cadence[k])
	}
	for _, k := range sortedKeys(rep.NextNumbers) {
		fmt.Fprintf(w, "next issue number %s: %d\n", k, rep.NextNumbers[k])
	}
	fmt.Fprintf(w, "files: uploaded %d, already in the store %d, a dry run would upload %d\n",
		rep.Uploads.Uploaded, rep.Uploads.Present, rep.Uploads.Planned)
	fmt.Fprintf(w, "uploads.linear.app URLs left as they are: %d\n", rep.PendingUploads)

	if len(rep.Verify) > 0 {
		fmt.Fprintln(w, "\nverify:")
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
		fmt.Fprintln(tw, "entity\texpected\tactual\t\t")
		for _, r := range rep.Verify {
			status := "ok"
			if r.Expected != r.Actual {
				status = "MISMATCH"
			}
			fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t\n", r.Entity, r.Expected, r.Actual, status)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		return PrintVerifyRows(w, rep.Verify)
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PrintVerifyRows lists the records a mismatched verify row names (ids
// only, never text).
func PrintVerifyRows(w io.Writer, rows []VerifyRow) error {
	for _, r := range rows {
		if r.Expected == r.Actual || len(r.Rows) == 0 {
			continue
		}
		if _, err := fmt.Fprintf(w, "%s: first %d of %d:\n", r.Entity, len(r.Rows), r.Actual); err != nil {
			return err
		}
		for _, x := range r.Rows {
			if _, err := fmt.Fprintf(w, "  %s\n", x); err != nil {
				return err
			}
		}
	}
	return nil
}
