package main

import (
	"fmt"
	"miniproject/api"
	"miniproject/auth"
)

func main() {
	fmt.Println(api.Handle(auth.Token("t")))
}
