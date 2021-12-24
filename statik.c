#include <unistd.h>
#include <stdlib.h>
#include <stdio.h>
#include <fcntl.h>

#ifdef DEBUG
#define verbose(format, ...) fprintf(stderr, "(verbose) "format, __VA_ARGS__)
#else
#define verbose(format, ...)
#endif

#define VERSION 1

char *body = "<html lang=\"en\"><head><title>%s</title></head><body>%s</body></html>";
char *line = "<li><a href=\"%s\">%s</a></li>";

void usage()
{
  printf("usage: statik [-r] [-b template] [-l template] [-v] [-h] [src] dest\n");
}

#define BUFFER_SIZE 4096
#define READ_SIZE 2048
// readall reads the give input file and allocates a buffer large enough to
// store its contents. Finally it returns the address of that buffer. Freeing
// is a duty of the caller
char *readall(const char *fp)
{
  size_t nch = 0, len, size = BUFFER_SIZE;
  char *buf;
  int fd;

  fd = open(fp, O_RDONLY);
  if(fd == -1) {
    perror("Could not read input template");
    exit(EXIT_FAILURE);
  }

  buf = malloc(sizeof(char) * size);
  if(buf == NULL) {
    perror("Could not allocate memory for input template");
    exit(EXIT_FAILURE);
  }

  // TODO: improve performance and use a buffered reader syscall
  while((len = read(fd, buf+nch, READ_SIZE)) != 0) {
    nch+=len;
    if(size-nch <= READ_SIZE) {
      size *= 2;
      buf = realloc(buf, size);
      if(buf == NULL) {
        perror("Could not allocate memory for input template");
        exit(EXIT_FAILURE);
      }
    }
  }
  realloc(buf, sizeof(char) * nch + 1);
  buf[nch++] = '\0';
  return buf;
}

int main(int argc, char *argv[])
{
  int recursive = 0;
  char *bd = NULL, *ln = NULL;
  char *src = ".", *dest = NULL;

  int opt;
  while ((opt = getopt(argc, argv, "rb:l:vh")) != -1) {
    switch (opt) {
      case 'r':
        recursive = 1;
        break;
      case 'b':
        bd = optarg;
        break;
      case 'l':
        ln = optarg;
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
        printf("Unkown flag: %c\n", opt);
        usage();
        exit(EXIT_FAILURE);
    }
  }

  // reqiure at least 2 arguments (src, dest)
  if (argc-optind > 2 || argc-optind < 1)  {
    printf("Wrong amount of arguments.\n");
    usage();
    exit(EXIT_FAILURE);
  }
  dest = argv[argc-1];
  if (argc-optind == 2)
    src = argv[argc-2];

  verbose("recursive=%d\n", recursive);
  verbose("bd=%s\n", bd);
  verbose("ln=%s\n", ln);
  verbose("src=%s\n", src);
  verbose("dest=%s\n", dest);

  if(bd != NULL)
    body = readall(bd);
  if(ln != NULL)
    line = readall(ln);

  verbose("body=%s\n", body);
  verbose("line=%s\n", line);

  if(bd != NULL)
    free(body);
  if(ln != NULL)
    free(line);
  exit(EXIT_SUCCESS);
}
