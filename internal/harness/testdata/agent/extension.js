// Synthetic public-safe pi extension for the Trygalle contract tests. It is
// installed into the hermetic agent directory by the harness; it never touches
// private harness names, paths, or credentials.
//
// Interaction surfaces the tests need:
// - /mock-dialog: opens a select dialog with a long timeout (answered by the
//   runtime with cancelled: true, contract R14).
// - /mock-notify: shows a fire-and-forget notification, starts no run.
// - /mock-run: starts its own run with pi.sendMessage({ triggerTurn: true })
//   from inside the command handler (contract R8: Pi emits agent_start before
//   the "handled" response).
// - session_before_switch: cancels a session switch only when the process env
//   MOCK_CANCEL_NEW_SESSION=1 (contract R13).
export default function (pi) {
	pi.registerCommand("mock-dialog", {
		description: "Open a select dialog",
		handler: async (_args, ctx) => {
			await ctx.ui.select("Mock dialog title", ["Option A", "Option B"], { timeout: 60000 });
		},
	});

	pi.registerCommand("mock-notify", {
		description: "Show a notification, starting no run",
		handler: async (_args, ctx) => {
			ctx.ui.notify("Mock notification", "info");
		},
	});

	pi.registerCommand("mock-run", {
		description: "Start its own run via sendMessage",
		handler: async (_args, ctx) => {
			pi.sendMessage({ customType: "mock-run", content: "mock run content" }, { triggerTurn: true });
		},
	});

	pi.on("session_before_switch", async (event) => {
		if (process.env.MOCK_CANCEL_NEW_SESSION === "1") {
			return { cancel: true };
		}
	});
}