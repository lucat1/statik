CC:=gcc
CFLAGS+=-Dversion=0 -Ddebug=0

statik: statik.o
	gcc -o statik $^

statik.o: statik.c

.PHONY: clean
clean:
	rm -f statik *.o
