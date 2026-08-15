# Implementation Notes: Replay Engine

During the implementation of the `replay` engine, several complex obstacles emerged that required careful debugging and architectural adjustments. These challenges highlight the nuances of building a robust, auditable execution engine in Go.

## 1. Stack Overflow from Recursive Retry Logic

**The Obstacle:** 
When implementing the "retry" error policy, the initial approach was to have the `applyErrorPolicy` function simply call `e.dispatch(ctx, step)` again. When running `TestRetryPolicy`, this immediately caused a stack overflow panic (exceeding Go's 1GB stack limit).

**The Hard Part:** 
The `dispatch` function catches errors and calls `applyErrorPolicy` to determine what to do. If `applyErrorPolicy` decides to retry by calling `dispatch`, and `dispatch` fails again calling `applyErrorPolicy`, it creates an infinite recursive loop. Because the mock browser in the test was instructed to always fail the click, the retry loop never exited, blowing the stack before the `MaxRetries` counter could be properly evaluated across the recursive frames.

**The Solution:** 
I refactored the retry block inside `applyErrorPolicy`. Instead of calling back up to `dispatch`, it now bypasses the routing layer and calls the underlying execution functions (`executeAction` or `executeAssert`) directly. This flattened the call stack, broke the recursive loop, and allowed the `MaxRetries` and `RetryDelayMs` logic to execute perfectly within a simple `for` loop.

---

## 2. Double-Appending "Skipped" Step Results

**The Obstacle:** 
In `TestSkipPolicy`, the engine was supposed to record that a step failed but was intentionally skipped, and then continue executing the plan. The test failed because the step results array contained duplicate entries.

**The Hard Part:** 
To ensure every step execution is audited regardless of panics or early returns, the `dispatch` function uses a `defer` block to guarantee a `StepResult` is appended to the `Run` log when the function exits. When I implemented the "skip" policy, I explicitly appended a `StepResult` with `Status: skipped` directly inside `applyErrorPolicy`. When the function returned, the `defer` block ran and appended a *second* result for the same step (with an empty status), causing duplicate logs with conflicting states.

**The Solution:** 
I removed the append logic from `applyErrorPolicy` entirely. Instead, when a step is skipped, `applyErrorPolicy` simply returns a `nil` error (indicating the policy successfully handled the failure). Back in `dispatch`, if an error occurred but `applyErrorPolicy` returned `nil`, the logic intercepts this state, marks the local `result.Status` as `skipped`, and allows the `defer` block to append exactly one clean audit record.

---

## 3. Branching `GOTO` Semantics vs. Linear Arrays

**The Obstacle:** 
The `TestBranchIfTrue` test failed because after successfully jumping to the correct target step, the engine unexpectedly executed the *next* step in the array (which was meant for the `IfFalse` path).

**The Hard Part:** 
The `Capability` schema defines steps as a flat JSON array. When a `Branch` evaluates a condition, it acts as a `GOTO` instruction, changing the loop index `i` to the target step's index. The difficulty was a mismatch in mental models: my unit test assumed that jumping to a step meant *only* executing that step and then halting. However, standard `GOTO` semantics dictate that once you jump to an index, execution resumes linearly from that new position. Because I had placed the `IfFalse` step sequentially after the `IfTrue` step in the test array, the engine naturally executed it next.

**The Solution:** 
The engine's execution logic was perfectly correct; the unit test was flawed. I redesigned the test's `Capability` array to mimic a real workflow. The test now verifies two specific behaviors:
1. That the engine correctly skipped over intermediate steps between the branch and the target.
2. That execution resumed normally from the target step onward. 
I removed the artificially placed `IfFalse` step from the linear path of the `IfTrue` execution, aligning the test perfectly with how a real `GOTO` operates over a flat array of instructions.
