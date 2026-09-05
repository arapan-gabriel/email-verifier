# Testing — unit

Table-driven. Classifier: `(code,text)→Class`. Pacer: sequences of samples → rate/conc/state
transitions, including "deferral does not move the pacer" and "throttle does". No real sockets or
Redis — both behind consumer-side interfaces (`ENGINEERING-STANDARDS.md` §2).

Anything involving elapsed time runs inside `synctest.Test`: a pauser that must hold an MX for five
minutes is verified without the suite taking five minutes, and without the flakiness a real clock
introduces under `-race`.
