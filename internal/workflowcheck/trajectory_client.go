package workflowcheck

import (
	"bytes"
	"encoding/json"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/ledger"
)

type trajectoryPageView struct {
	Entries    []ledger.Entry `json:"entries"`
	NextCursor string         `json:"next_cursor"`
	HasMore    bool           `json:"has_more"`
	Limit      int            `json:"limit"`
}

// trajectoryGet issues a GET against a raw trajectory path and decodes the
// paged response.
func (c *client) trajectoryGet(path string) (trajectoryPageView, response, error) {
	res, err := c.call("GET", path, nil, nil)
	if err != nil {
		return trajectoryPageView{}, response{}, err
	}
	if res.status != 200 {
		return trajectoryPageView{}, res, nil
	}
	var page trajectoryPageView
	if err := res.decode(&page); err != nil {
		return trajectoryPageView{}, res, err
	}
	return page, res, nil
}

// trajectoryExport issues a full NDJSON export and decodes each line.
func (c *client) trajectoryExport(path string) ([]ledger.Entry, response, error) {
	res, err := c.call("GET", path, nil, nil)
	if err != nil {
		return nil, response{}, err
	}
	if res.status != 200 {
		return nil, res, nil
	}
	var entries []ledger.Entry
	decoder := json.NewDecoder(bytes.NewReader(res.body))
	for decoder.More() {
		var entry ledger.Entry
		if err := decoder.Decode(&entry); err != nil {
			return nil, res, err
		}
		entries = append(entries, entry)
	}
	return entries, res, nil
}
