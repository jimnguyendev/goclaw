import { useState, useEffect, useMemo } from "react";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogFooter,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { Combobox } from "@/components/ui/combobox";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useProviders } from "@/pages/providers/hooks/use-providers";
import { useProviderModels } from "@/pages/providers/hooks/use-provider-models";
import { useProviderVerify } from "@/pages/providers/hooks/use-provider-verify";
import type { BuiltinToolData } from "./hooks/use-builtin-tools";

interface Props {
  tool: BuiltinToolData | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onSave: (name: string, settings: Record<string, unknown>) => Promise<void>;
}

const MEDIA_TOOLS = new Set(["read_image", "create_image"]);

export function BuiltinToolSettingsDialog({ tool, open, onOpenChange, onSave }: Props) {
  const isMedia = tool ? MEDIA_TOOLS.has(tool.name) : false;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        {isMedia && tool ? (
          <MediaSettingsForm tool={tool} onOpenChange={onOpenChange} onSave={onSave} />
        ) : (
          <JsonSettingsForm tool={tool} onOpenChange={onOpenChange} onSave={onSave} />
        )}
      </DialogContent>
    </Dialog>
  );
}

function MediaSettingsForm({
  tool,
  onOpenChange,
  onSave,
}: {
  tool: BuiltinToolData;
  onOpenChange: (open: boolean) => void;
  onSave: (name: string, settings: Record<string, unknown>) => Promise<void>;
}) {
  const { providers } = useProviders();
  const enabledProviders = providers.filter((p) => p.enabled);

  const settings = tool.settings ?? {};
  const [provider, setProvider] = useState((settings.provider as string) ?? "");
  const [model, setModel] = useState((settings.model as string) ?? "");
  const [saving, setSaving] = useState(false);

  // Resolve provider name → id for model list and verify
  const selectedProvider = useMemo(
    () => enabledProviders.find((p) => p.name === provider),
    [enabledProviders, provider],
  );
  const selectedProviderId = selectedProvider?.id;
  const { models, loading: modelsLoading } = useProviderModels(selectedProviderId, selectedProvider?.provider_type);
  const { verify, verifying, result: verifyResult, reset: resetVerify } = useProviderVerify();

  useEffect(() => {
    const s = tool.settings ?? {};
    setProvider((s.provider as string) ?? "");
    setModel((s.model as string) ?? "");
  }, [tool]);

  useEffect(() => {
    resetVerify();
  }, [provider, model, resetVerify]);

  const handleVerify = async () => {
    if (!selectedProviderId || !model.trim()) return;
    await verify(selectedProviderId, model.trim());
  };

  const handleSave = async () => {
    setSaving(true);
    try {
      const next: Record<string, unknown> = {};
      if (provider) next.provider = provider;
      if (model) next.model = model;
      await onSave(tool.name, next);
      onOpenChange(false);
    } finally {
      setSaving(false);
    }
  };

  return (
    <>
      <DialogHeader>
        <DialogTitle>{tool.display_name} Settings</DialogTitle>
        <DialogDescription>
          Configure the LLM provider and model. Leave empty for system defaults.
        </DialogDescription>
      </DialogHeader>
      <div className="space-y-4">
        <div className="space-y-2">
          <Label>Provider</Label>
          {enabledProviders.length > 0 ? (
            <Select
              value={provider}
              onValueChange={(v) => {
                setProvider(v);
                setModel("");
              }}
            >
              <SelectTrigger>
                <SelectValue placeholder="Select provider" />
              </SelectTrigger>
              <SelectContent>
                {enabledProviders.map((p) => (
                  <SelectItem key={p.name} value={p.name}>
                    {p.display_name || p.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          ) : (
            <Combobox
              value={provider}
              onChange={setProvider}
              options={[]}
              placeholder="No providers configured"
            />
          )}
        </div>
        <div className="space-y-2">
          <Label>Model</Label>
          <div className="flex gap-2">
            <div className="flex-1">
              <Combobox
                value={model}
                onChange={setModel}
                options={models.map((m) => ({ value: m.id, label: m.name }))}
                placeholder={modelsLoading ? "Loading models..." : "Enter or select model"}
              />
            </div>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="h-9 shrink-0 px-3"
              disabled={!selectedProviderId || !model.trim() || verifying}
              onClick={handleVerify}
            >
              {verifying ? "..." : "Check"}
            </Button>
          </div>
          {verifyResult && (
            <p className={`text-xs ${verifyResult.valid ? "text-success" : "text-destructive"}`}>
              {verifyResult.valid ? "Model verified" : verifyResult.error || "Verification failed"}
            </p>
          )}
        </div>
      </div>
      <DialogFooter>
        <Button variant="outline" onClick={() => onOpenChange(false)}>
          Cancel
        </Button>
        <Button onClick={handleSave} disabled={saving}>
          {saving ? "Saving..." : "Save"}
        </Button>
      </DialogFooter>
    </>
  );
}

function JsonSettingsForm({
  tool,
  onOpenChange,
  onSave,
}: {
  tool: BuiltinToolData | null;
  onOpenChange: (open: boolean) => void;
  onSave: (name: string, settings: Record<string, unknown>) => Promise<void>;
}) {
  const [json, setJson] = useState("");
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const [validJson, setValidJson] = useState(true);

  useEffect(() => {
    if (tool) {
      setJson(JSON.stringify(tool.settings ?? {}, null, 2));
      setError("");
      setValidJson(true);
    }
  }, [tool]);

  const handleJsonChange = (text: string) => {
    setJson(text);
    try {
      JSON.parse(text);
      setValidJson(true);
      setError("");
    } catch {
      setValidJson(false);
    }
  };

  const handleFormat = () => {
    try {
      const parsed = JSON.parse(json);
      setJson(JSON.stringify(parsed, null, 2));
      setError("");
      setValidJson(true);
    } catch {
      setError("Cannot format: invalid JSON");
    }
  };

  const handleSave = async () => {
    if (!tool) return;
    try {
      const parsed = JSON.parse(json);
      setSaving(true);
      setError("");
      await onSave(tool.name, parsed);
      onOpenChange(false);
    } catch (e) {
      setError(e instanceof SyntaxError ? "Invalid JSON" : String(e));
    } finally {
      setSaving(false);
    }
  };

  return (
    <>
      <DialogHeader>
        <DialogTitle>Settings: {tool?.display_name ?? tool?.name}</DialogTitle>
        <DialogDescription>
          Edit tool-specific settings as JSON. Changes take effect immediately after saving.
        </DialogDescription>
      </DialogHeader>
      <div className="space-y-3">
        <Textarea
          value={json}
          onChange={(e) => handleJsonChange(e.target.value)}
          rows={10}
          className={`font-mono text-sm ${!validJson ? "border-destructive" : ""}`}
        />
        <div className="flex items-center justify-between">
          <Button variant="ghost" size="sm" onClick={handleFormat} className="h-7 px-2 text-xs">
            Format JSON
          </Button>
          {!validJson && <span className="text-xs text-destructive">Invalid JSON syntax</span>}
        </div>
        {error && <p className="text-sm text-destructive">{error}</p>}
      </div>
      <DialogFooter>
        <Button variant="outline" onClick={() => onOpenChange(false)}>
          Cancel
        </Button>
        <Button onClick={handleSave} disabled={saving || !validJson}>
          {saving ? "Saving..." : "Save"}
        </Button>
      </DialogFooter>
    </>
  );
}
