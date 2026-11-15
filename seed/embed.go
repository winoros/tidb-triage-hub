package seedassets

import _ "embed"

// InitialDatasetJSON embeds the initial seed snapshot as a fallback when seed/initial_dataset.json is absent on disk.
//
//go:embed initial_dataset.json
var InitialDatasetJSON []byte
