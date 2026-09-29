import {writeFile} from "node:fs/promises";

export async function writeFixtureConfig(
  filePath: string,
  workspace?: string,
  tools = true
): Promise<string> {
  const fields = [
    "[execution]",
    'provider = "openai"',
    'model = "fixture-model"',
    `tools = ${tools}`
  ];
  if (workspace !== undefined) fields.push(`workspace = ${JSON.stringify(workspace)}`);
  await writeFile(filePath, fields.join("\n") + "\n", {mode: 0o600});
  return filePath;
}
