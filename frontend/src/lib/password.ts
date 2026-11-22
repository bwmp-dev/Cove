export function passwordError(password: string, confirmation: string) {
  if (password !== confirmation) return "The passwords do not match.";
  const bytes = new TextEncoder().encode(password).length;
  if (bytes < 16 || bytes > 72) return "Use a password containing 16 to 72 bytes.";
  return null;
}
