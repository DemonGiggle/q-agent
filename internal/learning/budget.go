package learning

// Reserve returns the cumulative usage when all dimensions fit the limit.
// Depth measures the deepest reached level and other units accumulate.
func (b Budget) Reserve(cost, limit Budget) (Budget, bool) {
	add := func(spent, charge, cap int64) (int64, bool) {
		if spent < 0 || cap < spent || charge < 0 || charge > cap-spent {
			return 0, false
		}
		return spent + charge, true
	}
	requests, rq := add(b.Requests, cost.Requests, limit.Requests)
	documents, doc := add(b.Documents, cost.Documents, limit.Documents)
	tokens, tok := add(b.Tokens, cost.Tokens, limit.Tokens)
	micros, usd := add(b.CostMicros, cost.CostMicros, limit.CostMicros)
	wall, ms := add(b.WallTimeMillis, cost.WallTimeMillis, limit.WallTimeMillis)
	storage, bytes := add(b.StorageBytes, cost.StorageBytes, limit.StorageBytes)
	depth := max(b.Depth, cost.Depth)
	if !(rq && doc && tok && usd && ms && bytes) || b.Depth < 0 || cost.Depth < 0 || depth > limit.Depth {
		return Budget{}, false
	}
	return Budget{Requests: requests, Documents: documents, Tokens: tokens, CostMicros: micros,
		WallTimeMillis: wall, StorageBytes: storage, Depth: depth}, true
}
