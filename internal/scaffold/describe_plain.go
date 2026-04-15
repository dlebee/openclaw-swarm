package scaffold

import (
	"fmt"
	"strings"
)

func describePlain(compiled []compiledPhase) string {
	var b strings.Builder
	for _, ph := range compiled {
		fmt.Fprintf(&b, "phase %q (concurrency=%d)\n", ph.name, ph.concurrency)
		for _, st := range ph.steps {
			fmt.Fprintf(&b, "  step %q: %d cells", st.name, len(st.cells))
			if st.barrier != nil {
				b.WriteString(" [barrier]")
			}
			b.WriteByte('\n')
			for _, c := range st.cells {
				fmt.Fprintf(&b, "    - %s @ %s\n", c.action.Name(), c.target.ID)
			}
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}
