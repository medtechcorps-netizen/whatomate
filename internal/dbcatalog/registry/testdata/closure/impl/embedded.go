package impl

// Both embedded targets already implement Runner; changing the embedding must
// still change the hash even when the union of resolved method bodies is equal.
type Embedded struct{ One }
