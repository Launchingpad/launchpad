package backupapi

import (
	"encoding/json"
	"fmt"
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func fmtSprint(v any) string            { return fmt.Sprint(v) }
func fmtSprintf(f string, v any) string { return fmt.Sprintf(f, v) }
