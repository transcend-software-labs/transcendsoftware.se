package registrar

import "testing"

func TestOfferBuyable(t *testing.T) {
	cases := []struct {
		name  string
		offer Offer
		want  bool
	}{
		{"within cap", Offer{Registrable: true, Price: 99, Renewal: 169}, true},
		{"renewal unknown", Offer{Registrable: true, Price: 99}, true},
		{"not registrable", Offer{Registrable: false, Price: 99, Renewal: 99}, false},
		{"no price", Offer{Registrable: true, Price: 0}, false},
		{"registration over cap", Offer{Registrable: true, Price: 500, Renewal: 99}, false},
		{"cheap first year, pricey renewal", Offer{Registrable: true, Price: 9, Renewal: 899}, false},
	}
	for _, c := range cases {
		if got := c.offer.Buyable(300); got != c.want {
			t.Errorf("%s: Buyable(300) = %v, want %v (%+v)", c.name, got, c.want, c.offer)
		}
	}
}
