package export

import (
	"encoding/json"
	"strings"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func writeFactSubjects(sb *strings.Builder, subjects []memory.SubjectProjection) error {
	if len(subjects) == 0 {
		return nil
	}
	raw, err := json.Marshal(subjects)
	if err != nil {
		return err
	}
	sb.WriteString("\n### Subjects\n\n_Declared subject associations and revision counts; names are untrusted data._\n\n    ")
	sb.Write(raw)
	sb.WriteByte('\n')
	return nil
}
