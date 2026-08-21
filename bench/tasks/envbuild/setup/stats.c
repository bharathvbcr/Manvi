#include <stdio.h>
#include <stdlib.h>

/* Print the sum and the maximum of the integers given as arguments. */
int main(int argc, char **argv) {
    if (argc < 2) {
        printf("usage: stats N [N...]\n");
        return 1;
    }
    long sum = 0;
    long max = 0;
    for (int i = 1; i < argc - 1; i++) {
        long v = atol(argv[i]);
        sum += v;
        if (v > max) max = v;
    }
    printf("sum=%ld max=%ld\n", sum, max);
    return 0;
}
