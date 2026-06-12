# Non-interactive R exits "Execution halted" on an uncaught interrupt (SIGINT
# outside .repld_run's tryCatch: pre-handler decode/parse, sentinel statement)
# unless this is set (check_session_exit, R src/main/main.c).
options(catch.script.errors = TRUE)

.repld_decode <- function(encoded) {
  if (!nzchar(encoded)) {
    return("")
  }
  starts <- seq.int(1L, nchar(encoded), by = 2L)
  bytes <- substring(encoded, starts, starts + 1L)
  gsub("\r\n?", "\n", rawToChar(as.raw(strtoi(bytes, 16L))))
}

.repld_hex <- function(text) {
  paste(format(charToRaw(enc2utf8(text))), collapse = "")
}

.repld_connect <- function() {
  addr <- Sys.getenv("REPLD_CONTROL_ADDR", "")
  token <- Sys.getenv("REPLD_CONTROL_TOKEN", "")
  if (!nzchar(addr) || !nzchar(token)) {
    return(NULL)
  }
  parts <- strsplit(addr, ":", fixed = TRUE)[[1]]
  port <- suppressWarnings(as.integer(parts[length(parts)]))
  host <- paste(parts[-length(parts)], collapse = ":")
  if (!nzchar(host) || is.na(port)) {
    return(NULL)
  }
  tryCatch({
    con <- socketConnection(host, port = port, open = "r+b", blocking = TRUE)
    writeLines(token, con)
    flush(con)
    con
  }, error = function(e) NULL)
}

.repld_control <- .repld_connect()

.repld_write_control <- function(line) {
  if (is.null(.repld_control)) {
    return(invisible(NULL))
  }
  try({
    writeLines(line, .repld_control)
    flush(.repld_control)
  }, silent = TRUE)
  invisible(NULL)
}

.repld_render_error <- function(err) {
  short <- paste0("ERROR: ", conditionMessage(err))
  call <- conditionCall(err)
  if (is.null(call)) {
    smart <- short
  } else {
    smart <- paste0(short, "\nCall: ", paste(deparse(call), collapse = " "), "\n")
  }
  full <- paste(capture.output(print(err)), collapse = "\n")
  c(short, smart, full)
}

.repld_run <- function(hex_code, print_result) {
  # Engine resends SIGINT while an interrupt is in flight; a late one sits
  # pending at the top-level read and would detonate inside this eval. Discard
  # it (a real interrupt for this eval gets resent).
  tryCatch(Sys.sleep(0.001), interrupt = function(cond) NULL)
  tryCatch({
    code <- .repld_decode(hex_code)
    exprs <- parse(text = code, srcfile = srcfilecopy("<repld>", code))
    visible <- FALSE
    value <- NULL
    for (expr in exprs) {
      result <- withVisible(eval(expr, envir = .GlobalEnv))
      visible <- result$visible
      value <- result$value
    }
    if (isTRUE(visible)) {
      print(value)
    }
    flush(stdout())
    .repld_write_control("OK")
  }, error = function(err) {
    flush(stdout())
    rendered <- .repld_render_error(err)
    .repld_write_control(paste(
      "ERR",
      .repld_hex(rendered[[1L]]),
      .repld_hex(rendered[[2L]]),
      .repld_hex(rendered[[3L]])
    ))
  # R has no control-socket interrupt listener; the engine delivers SIGINT, which
  # raises an "interrupt" condition (NOT caught by error=). Catch it here so the
  # eval aborts but the read loop survives to emit the sentinel and stay reusable.
  }, interrupt = function(cond) {
    flush(stdout())
    short <- "ERROR: interrupted"
    .repld_write_control(paste(
      "ERR",
      .repld_hex(short),
      .repld_hex(paste0(short, "\n")),
      .repld_hex(paste0(short, "\n"))
    ))
  })
  invisible(NULL)
}
