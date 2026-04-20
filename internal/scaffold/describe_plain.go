package scaffold

import (
	"context"
	"fmt"
	"strings"
)

func describePlainWithProbe(ctx context.Context, compiled []compiledPhase, h PlanDisplayHints) (string, error) {
	cells, err := annotatePlanCellsWithProbe(ctx, compiled, h)
	if err != nil {
		return "", err
	}
	segs := planSegmentsFromCells(cells)
	var b strings.Builder
	b.WriteString("Prepared plan\n\n")
	for _, ph := range segs {
		fmt.Fprintf(&b, "Phase: %s\n", ph.phase)
		for _, tg := range ph.targets {
			fmt.Fprintf(&b, "  Target %s\n", tg.id)
			for _, c := range tg.cells {
				timing := StepTimingPlain(c)
				fmt.Fprintf(&b, "    %s  %s%s\n", stepDisplayName(c.step), cellStatusText(c), timing)
			}
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String()), nil
}
