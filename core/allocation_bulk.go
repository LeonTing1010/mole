package core

import (
	"fmt"
	"net/http"
)

func allocationBulk(client *http.Client, hy2Lanes, bytesPerStream int) (float64, error) {
	if hy2Lanes < 0 || hy2Lanes > BalanceLanes {
		return 0, fmt.Errorf("invalid UDP share %d", hy2Lanes)
	}
	urls := allocationURLs(hy2Lanes)

	return bulkURLsWithClient(client, urls, bytesPerStream)
}

func allocationURLs(hy2Lanes int) []string {
	urls := make([]string, BalanceLanes)
	for lane := range urls {
		port := BrutalKeeperPort
		if (lane+1)*hy2Lanes/BalanceLanes > lane*hy2Lanes/BalanceLanes {
			port = LegGatePort
		}
		urls[lane] = fmt.Sprintf("http://127.0.0.1:%d", port)
	}
	return urls
}
