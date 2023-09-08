bin/meyerson: *.go
	go build -o $@ $^

meyerson: bin/meyerson
