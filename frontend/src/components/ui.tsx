import { forwardRef, type ButtonHTMLAttributes, type ReactNode } from "react";
import * as DialogPrimitive from "@radix-ui/react-dialog";
import * as AlertDialogPrimitive from "@radix-ui/react-alert-dialog";
import * as DropdownPrimitive from "@radix-ui/react-dropdown-menu";
import * as SelectPrimitive from "@radix-ui/react-select";
import * as SwitchPrimitive from "@radix-ui/react-switch";
import { AlertCircle, Check, ChevronDown, X } from "lucide-react";
import { cn } from "../lib/utils";

export const Button = forwardRef<
  HTMLButtonElement,
  ButtonHTMLAttributes<HTMLButtonElement> & {
    variant?: "default" | "outline" | "ghost" | "destructive" | "secondary";
    size?: "default" | "sm" | "icon";
  }
>(function Button(
  {
    className,
    variant = "default",
    size = "default",
    type = "button",
    ...props
  },
  ref,
) {
  return (
    <button
      ref={ref}
      type={type}
      className={cn(
        "button",
        `button-${variant}`,
        size !== "default" && `button-${size}`,
        className,
      )}
      {...props}
    />
  );
});
export function Badge({
  children,
  tone = "secondary",
  className,
}: {
  children: ReactNode;
  tone?: string;
  className?: string;
}) {
  return (
    <span className={cn("badge", `badge-${tone}`, className)}>{children}</span>
  );
}
export function Alert({
  children,
  tone = "destructive",
  id,
}: {
  children: ReactNode;
  tone?: string;
  id?: string;
}) {
  return (
    <div
      id={id}
      role={tone === "destructive" ? "alert" : "status"}
      className={`alert alert-${tone}`}
    >
      <AlertCircle aria-hidden="true" />
      <div>{children}</div>
    </div>
  );
}
export function Empty({
  icon,
  title,
  children,
  action,
  id,
}: {
  icon?: ReactNode;
  title: string;
  children: ReactNode;
  action?: ReactNode;
  id?: string;
}) {
  return (
    <div id={id} className="empty-state">
      <div className="empty-icon">{icon}</div>
      <h3>{title}</h3>
      <p>{children}</p>
      {action}
    </div>
  );
}
export function TableWrap({ children }: { children: ReactNode }) {
  return <div className="table-wrap">{children}</div>;
}
export function SelectField({
  id,
  label,
  value,
  onValueChange,
  options,
  disabled,
  placeholder,
}: {
  id?: string;
  label: string;
  value: string;
  onValueChange: (value: string) => void;
  options: { value: string; label: string; disabled?: boolean }[];
  disabled?: boolean;
  placeholder?: string;
}) {
  return (
    <SelectPrimitive.Root
      value={value}
      onValueChange={onValueChange}
      disabled={disabled}
    >
      <SelectPrimitive.Trigger
        id={id}
        aria-label={label}
        className="select-trigger"
      >
        <SelectPrimitive.Value placeholder={placeholder || label} />
        <SelectPrimitive.Icon>
          <ChevronDown size={15} />
        </SelectPrimitive.Icon>
      </SelectPrimitive.Trigger>
      <SelectPrimitive.Portal>
        <SelectPrimitive.Content
          className="select-content"
          position="popper"
          sideOffset={5}
        >
          <SelectPrimitive.Viewport>
            {options.map((item) => (
              <SelectPrimitive.Item
                className="select-item"
                key={item.value}
                value={item.value}
                disabled={item.disabled}
              >
                <SelectPrimitive.ItemText>
                  {item.label}
                </SelectPrimitive.ItemText>
                <SelectPrimitive.ItemIndicator>
                  <Check size={15} />
                </SelectPrimitive.ItemIndicator>
              </SelectPrimitive.Item>
            ))}
          </SelectPrimitive.Viewport>
        </SelectPrimitive.Content>
      </SelectPrimitive.Portal>
    </SelectPrimitive.Root>
  );
}
export function Switch({
  id,
  checked,
  onCheckedChange,
  label,
  disabled,
}: {
  id?: string;
  checked: boolean;
  onCheckedChange: (value: boolean) => void;
  label: string;
  disabled?: boolean;
}) {
  return (
    <SwitchPrimitive.Root
      id={id}
      aria-label={label}
      className="switch"
      checked={checked}
      onCheckedChange={onCheckedChange}
      disabled={disabled}
    >
      <SwitchPrimitive.Thumb className="switch-thumb" />
    </SwitchPrimitive.Root>
  );
}
export function Modal({
  open,
  onOpenChange,
  title,
  description,
  children,
  footer,
  id,
  sheet = false,
  closeDisabled = false,
  titleID,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description: string;
  children: ReactNode;
  footer?: ReactNode;
  id?: string;
  sheet?: boolean;
  closeDisabled?: boolean;
  titleID?: string;
}) {
  return (
    <DialogPrimitive.Root open={open} onOpenChange={onOpenChange}>
      <DialogPrimitive.Portal>
        <DialogPrimitive.Overlay className="dialog-overlay" />
        <DialogPrimitive.Content
          id={id}
          className={cn("dialog-content", sheet && "sheet-content")}
          onEscapeKeyDown={(event) => {
            if (closeDisabled) event.preventDefault();
          }}
          onInteractOutside={(event) => {
            if (closeDisabled) event.preventDefault();
          }}
        >
          <div className="dialog-heading">
            <DialogPrimitive.Title id={titleID}>{title}</DialogPrimitive.Title>
            <DialogPrimitive.Description>
              {description}
            </DialogPrimitive.Description>
          </div>
          <DialogPrimitive.Close asChild>
            <Button
              variant="ghost"
              size="icon"
              className="dialog-close"
              aria-label="关闭"
              disabled={closeDisabled}
            >
              <X size={18} />
            </Button>
          </DialogPrimitive.Close>
          <div className="dialog-body">{children}</div>
          {footer && <div className="dialog-footer">{footer}</div>}
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  );
}
export interface ConfirmState {
  title: string;
  description: string;
  label: string;
  danger?: boolean;
  onConfirm: () => void;
}
export function ConfirmDialog({
  state,
  onClose,
}: {
  state: ConfirmState | null;
  onClose: () => void;
}) {
  return (
    <AlertDialogPrimitive.Root
      open={!!state}
      onOpenChange={(value) => {
        if (!value) onClose();
      }}
    >
      <AlertDialogPrimitive.Portal>
        <AlertDialogPrimitive.Overlay className="dialog-overlay confirm-overlay" />
        <AlertDialogPrimitive.Content
          id="confirm-dialog"
          className="dialog-content confirm-content"
        >
          <div className="dialog-heading">
            <AlertDialogPrimitive.Title id="confirm-title">
              {state?.title}
            </AlertDialogPrimitive.Title>
            <AlertDialogPrimitive.Description id="confirm-description">
              {state?.description}
            </AlertDialogPrimitive.Description>
          </div>
          <div className="dialog-footer">
            <AlertDialogPrimitive.Cancel asChild>
              <Button id="confirm-cancel" variant="outline">
                取消
              </Button>
            </AlertDialogPrimitive.Cancel>
            <AlertDialogPrimitive.Action asChild>
              <Button
                id="confirm-ok"
                variant={state?.danger ? "destructive" : "default"}
                onClick={() => state?.onConfirm()}
              >
                {state?.label}
              </Button>
            </AlertDialogPrimitive.Action>
          </div>
        </AlertDialogPrimitive.Content>
      </AlertDialogPrimitive.Portal>
    </AlertDialogPrimitive.Root>
  );
}
export const Menu = DropdownPrimitive.Root;
export const MenuTrigger = DropdownPrimitive.Trigger;
export function MenuContent({
  children,
  align = "end",
}: {
  children: ReactNode;
  align?: "start" | "end";
}) {
  return (
    <DropdownPrimitive.Portal>
      <DropdownPrimitive.Content
        className="menu-content"
        align={align}
        sideOffset={6}
      >
        {children}
      </DropdownPrimitive.Content>
    </DropdownPrimitive.Portal>
  );
}
export function MenuItem({
  children,
  onSelect,
  danger = false,
  disabled = false,
}: {
  children: ReactNode;
  onSelect?: () => void;
  danger?: boolean;
  disabled?: boolean;
}) {
  return (
    <DropdownPrimitive.Item
      className={cn("menu-item", danger && "menu-danger")}
      onSelect={onSelect}
      disabled={disabled}
    >
      {children}
    </DropdownPrimitive.Item>
  );
}
export function MenuLabel({ children }: { children: ReactNode }) {
  return (
    <DropdownPrimitive.Label className="menu-label">
      {children}
    </DropdownPrimitive.Label>
  );
}
export const MenuSeparator = () => (
  <DropdownPrimitive.Separator className="menu-separator" />
);
