# Cleans `go test -bench` output. The example server logs JSON lines to stdout
# in the middle of a benchmark row. This drops the log text and joins the row.
/^\{"level"/ { next }
{ sub(/\{"level".*$/, "") }
/^Benchmark/ && /\t$/ { held = $0; next }
held != "" && /^ *[0-9]/ { print held $0; held = ""; next }
{ print }
