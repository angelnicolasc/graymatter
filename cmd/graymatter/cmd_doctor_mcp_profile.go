package main

import (
	"fmt"

	gmcp "github.com/angelnicolasc/graymatter/cmd/graymatter/internal/mcp"
)

func checkMCPCatalog(profile string) checkResult {
	result := checkResult{Name: "mcp catalog"}
	cost, err := gmcp.CatalogCost(profile)
	if err != nil {
		result.Status = "fail"
		result.Detail = err.Error()
		return result
	}
	result.Status = "info"
	result.Detail = fmt.Sprintf("%s selected profile: %d estimated fixed tokens (%d UTF-8 bytes); instructions + tools/list", profile, cost.Tokens, cost.Bytes)
	result.Hint = "core is the default; use doctor --tools full to estimate full. Local tokens.Approx estimate, not provider billing; does not inspect other processes' argv"
	return result
}
