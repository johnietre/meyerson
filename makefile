.PHONY: meyerson

meyerson:
	go build -o bin/$@ cmd/meyerson/*.go
