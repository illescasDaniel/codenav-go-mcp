package main

import (
	"fmt"

	"example.com/shop/cart"
)

func main() {
	var c cart.Cart
	c.Add(cart.Item{SKU: "pen", Quantity: 2, Cents: 150})
	fmt.Println(c.Total())

	var p cart.Pricer = cart.MapPricer{"pen": 150}
	if cents, ok := p.Price("pen"); ok {
		fmt.Println(cents)
	}
}
