import { useState, useEffect } from "react";
import { Lock, RotateCcw, Sparkles } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogFooter,
} from "@/components/ui/dialog";
import { useAuthStore } from "@/stores/use-auth-store";
import type { AgentData, BootstrapFile } from "@/types/agent";
import {
  FileSidebar,
  FileEditor,
  RegenerateDialog,
  OpenAgentEmptyState,
} from "./file-sections";

interface AgentFilesTabProps {
  agent: AgentData;
  files: BootstrapFile[];
  onGetFile: (name: string) => Promise<BootstrapFile | null>;
  onSetFile: (name: string, content: string) => Promise<void>;
  onRegenerate?: (prompt: string) => Promise<void>;
  onResummon?: () => Promise<void>;
}

export function AgentFilesTab({
  agent,
  files,
  onGetFile,
  onSetFile,
  onRegenerate,
  onResummon,
}: AgentFilesTabProps) {
  const userId = useAuthStore((s) => s.userId);
  const [selectedFile, setSelectedFile] = useState<string | null>(null);
  const [content, setContent] = useState("");
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const [dirty, setDirty] = useState(false);
  const [regenerateOpen, setRegenerateOpen] = useState(false);
  const [resummonOpen, setResummonOpen] = useState(false);
  const [resummonning, setResummonning] = useState(false);

  const isOpen = agent.agent_type === "open";
  const isPredefined = agent.agent_type === "predefined";
  const isOwner = agent.owner_id === userId;
  const canEdit = !isPredefined || isOwner;

  // Hide MEMORY.json and BOOTSTRAP.md for predefined agents
  const displayFiles = files.filter(
    (f) =>
      f.name !== "MEMORY.json" &&
      !(isPredefined && f.name === "BOOTSTRAP.md"),
  );
  const allMissing =
    displayFiles.length > 0 && displayFiles.every((f) => f.missing);

  const isUserScoped = (fileName: string) =>
    isPredefined && fileName === "USER.md";

  useEffect(() => {
    if (!selectedFile) return;
    setLoading(true);
    onGetFile(selectedFile)
      .then((file) => {
        setContent(file?.content ?? "");
        setDirty(false);
      })
      .finally(() => setLoading(false));
  }, [selectedFile, onGetFile]);

  const handleSave = async () => {
    if (!selectedFile) return;
    setSaving(true);
    try {
      await onSetFile(selectedFile, content);
      setDirty(false);
    } finally {
      setSaving(false);
    }
  };

  const handleContentChange = (val: string) => {
    setContent(val);
    setDirty(true);
  };

  const handleResummon = async () => {
    if (!onResummon) return;
    setResummonning(true);
    try {
      await onResummon();
      setResummonOpen(false);
    } finally {
      setResummonning(false);
    }
  };

  if (isOpen && allMissing) {
    return <OpenAgentEmptyState files={displayFiles} />;
  }

  const aiActions = isPredefined && isOwner && (
    <>
      {onResummon && (
        <Button
          variant="outline"
          size="sm"
          onClick={() => setResummonOpen(true)}
          className="gap-1.5"
        >
          <RotateCcw className="h-3.5 w-3.5" />
          Resummon
        </Button>
      )}
      {onRegenerate && (
        <Button
          variant="outline"
          size="sm"
          onClick={() => setRegenerateOpen(true)}
          className="gap-1.5"
        >
          <Sparkles className="h-3.5 w-3.5" />
          Edit with AI
        </Button>
      )}
    </>
  );

  return (
    <div className="space-y-3">
      {isPredefined && !isOwner && (
        <div className="flex items-start gap-3 rounded-lg border border-amber-500/30 bg-amber-500/5 p-4">
          <Lock className="mt-0.5 h-5 w-5 shrink-0 text-amber-600 dark:text-amber-400" />
          <div className="text-sm">
            <p className="font-medium">Read-only</p>
            <p className="text-muted-foreground">
              Only the agent owner can edit predefined context files.
            </p>
          </div>
        </div>
      )}

      <div className="flex h-[600px] gap-3">
        <FileSidebar
          files={displayFiles}
          selectedFile={selectedFile}
          onSelect={setSelectedFile}
          isUserScoped={isUserScoped}
        />
        <FileEditor
          fileName={selectedFile}
          content={content}
          onChange={handleContentChange}
          loading={loading}
          dirty={dirty}
          saving={saving}
          canEdit={canEdit}
          onSave={handleSave}
          headerActions={aiActions || undefined}
        />
      </div>

      {onRegenerate && (
        <RegenerateDialog
          open={regenerateOpen}
          onOpenChange={setRegenerateOpen}
          onRegenerate={onRegenerate}
        />
      )}

      <Dialog open={resummonOpen} onOpenChange={setResummonOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle className="flex items-center gap-2">
              <RotateCcw className="h-4 w-4" />
              Resummon Agent
            </DialogTitle>
          </DialogHeader>
          <p className="text-sm text-muted-foreground">
            This will regenerate <strong>SOUL.md</strong>, <strong>IDENTITY.md</strong>, and
            optionally <strong>USER_PREDEFINED.md</strong> from scratch using the original
            description. Any manual edits to these files will be overwritten.
          </p>
          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => setResummonOpen(false)}
              disabled={resummonning}
            >
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={handleResummon}
              disabled={resummonning}
              className="gap-1.5"
            >
              <RotateCcw className="h-3.5 w-3.5" />
              {resummonning ? "Summoning..." : "Resummon"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
