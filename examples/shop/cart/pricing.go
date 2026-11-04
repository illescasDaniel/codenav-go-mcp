package cart

// Pricer looks up the price of a SKU in cents.
type Pricer interface {
	Price(sku string) (cents int64, ok bool)
}

// MapPricer is a Pricer backed by a map.
type MapPricer map[string]int64

// Price implements Pricer.
func (m MapPricer) Price(sku string) (int64, bool) {
	cents, ok := m[sku]
	return cents, ok
}
