#include <unistd.h>
#include <stdlib.h>
#include <stdio.h>

#define VERSION 1

void usage()
{
  printf("usage: statik [-r] [-t template] [-v] [-h] [src] dest\n");
}

int main(int argc, char *argv[])
{
  int recursive = 0;
  char *template = NULL;
  char *src = ".", *dest = NULL;

  int opt;
  while ((opt = getopt(argc, argv, "rt:vh")) != -1) {
    switch (opt) {
      case 'r':
        recursive = 1;
        break;
      case 't':
        template = optarg;
        break;
      case 'v':
        printf("version: %d\n", VERSION);
        exit(EXIT_SUCCESS);
        break;
      case 'h':
        usage();
        exit(EXIT_SUCCESS);
        break;
      default: // unkown flag
        printf("Unkown flag: %s\n", opt);
        usage();
        exit(EXIT_FAILURE);
    }
  }

  // reqiure at least 2 arguments (src, dest)
  if (argc-optind > 2 || argc-optind < 1)  {
    printf("Wrong amount of arguments.\n", argc-optind);
    usage();
    exit(EXIT_FAILURE);
  }
  dest = argv[argc-1];
  if (argc-optind == 2)
    src = argv[argc-2];

  printf("recursive=%d\n", recursive);
  printf("template=%s\n", template);
  printf("src=%s, dest=%s\n", src, dest);

  exit(EXIT_SUCCESS);
}
