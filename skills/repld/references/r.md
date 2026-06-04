# R (`repld R`)

Persistent R session. `R` below is the interpreter, or an explicit path to one.

## Usage

```bash
repld R -e 'x <- rnorm(100)'       # eval
repld R -e 'mean(x)'               # visible results print like R

repld --session stats R -e 'fit <- lm(mpg ~ wt, mtcars)'
repld --session stats R -e 'coef(fit)'
```

R runs as `R --slave --no-save --no-restore`. State in `.GlobalEnv` persists across calls. To run a file, use normal R code:

```bash
repld R -e 'source("analysis.R")'
```

Interrupt support is currently best-effort; reset with `--fresh` when a session is wedged.
