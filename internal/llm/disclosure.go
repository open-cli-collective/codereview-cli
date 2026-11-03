package llm

// SensitiveValueRule is the output rule carried by every prompt whose
// model-written text cr posts or logs. Review text on a public repository is
// public, so a finding that quotes a leaked value republishes the leak. The
// rule lives in the prompts rather than in a redactor because only the model
// reading the change can tell a real account number from an example one.
const SensitiveValueRule = "Never reproduce a suspected credential, secret, or personal or financial identifier or figure from any input (the diff, repository files, PR discussion, or earlier review text) in any response field, even when that value is the subject of the text: cr publishes this text, so quoting the value republishes it. " +
	"This covers API tokens, private keys and passwords; account, card, routing, tax and government ID numbers; balances, amounts and income figures; and a real person's name, home address, email address, phone number or date of birth. " +
	"Name the kind of value and its location instead (file, line, JSON path or key), and if a fragment is needed, mask it to at most its last 2 characters, for example \"account number ending ..21\"."
