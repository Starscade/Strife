CREATE OR REPLACE FUNCTION public.pgrst_notify()
RETURNS event_trigger AS $$
BEGIN
	NOTIFY pgrst, 'reload schema';
END;
$$ LANGUAGE plpgsql;

CREATE EVENT TRIGGER pgrst_reload_on_ddl ON ddl_command_end
	EXECUTE FUNCTION public.pgrst_notify();

CREATE EVENT TRIGGER pgrst_reload_on_drop ON sql_drop
	EXECUTE FUNCTION public.pgrst_notify();
