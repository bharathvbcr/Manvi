`sh test_stats.sh` fails.

There are two independent problems: the build does not run on this machine, and
`stats.c` computes the wrong answer. Fix both so that `sh test_stats.sh` prints
`ok`.

`stats N [N...]` must print `sum=<sum> max=<max>` over **all** the integers given
as arguments. Do not modify `test_stats.sh`.
