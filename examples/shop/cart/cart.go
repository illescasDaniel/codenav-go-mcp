// Package cart holds a customer's basket.
package cart

// Cart is a customer's basket. The zero value is an empty cart.
type Cart struct {
	Items    []Item
	Currency string
}

// Item is one line in a Cart.
type Item struct {
	SKU      string
	Quantity int
	Cents    int64
}

// Total returns the cart total in cents.
func (c *Cart) Total() int64 {
	var sum int64
	for _, it := range c.Items {
		sum += int64(it.Quantity) * it.Cents
	}
	return sum
}

// Add puts an item in the cart.
func (c *Cart) Add(it Item) { c.Items = append(c.Items, it) }
