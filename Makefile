CC:=gcc
CFLAGS+=-Wall -pedantic

statik: statik.o
	gcc -o statik $^

debug: CFLAGS+=-DDEBUG=1 -ggdb
debug: statik

statik.o: statik.c

.PHONY: clean
clean:
	rm -f statik *.o
