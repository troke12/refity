import { useState, useEffect } from 'react';
import { useNavigate } from 'react-router-dom';
import { Link } from 'react-router-dom';
import { dashboardAPI, authAPI } from '../services/api';
import { formatBytes } from '../utils/formatBytes';
import StatCard from '../components/StatCard';
import CreateGroupModal from '../components/CreateGroupModal';
import './Dashboard.css';

function Dashboard() {
  const [data, setData] = useState(null);
  const [isLoading, setIsLoading] = useState(true);
  const [isRefreshing, setIsRefreshing] = useState(false);
  const [showCreateGroupModal, setShowCreateGroupModal] = useState(false);
  const navigate = useNavigate();

  const loadData = async () => {
    try {
      const dashboardData = await dashboardAPI.get();
      setData(dashboardData);
    } catch (error) {
      console.error('Failed to load dashboard:', error);
    } finally {
      setIsLoading(false);
      setIsRefreshing(false);
    }
  };

  useEffect(() => {
    loadData();
  }, []);

  const handleRefresh = () => {
    setIsRefreshing(true);
    loadData();
  };

  const handleLogout = async () => {
    if (window.confirm('Are you sure you want to logout?')) {
      await authAPI.logout();
      navigate('/login');
    }
  };

  const handleGroupCreated = () => {
    setShowCreateGroupModal(false);
    loadData();
  };


  if (isLoading) {
    return (
      <div className="d-flex justify-content-center align-items-center" style={{ minHeight: '100vh' }}>
        <div className="spinner-border text-primary" role="status">
          <span className="visually-hidden">Loading...</span>
        </div>
      </div>
    );
  }

  return (
    <div className="container-main">
      <nav className="navbar navbar-expand-lg navbar-dark mb-4">
        <div className="container-fluid">
          <span className="navbar-brand">
            <i className="bi bi-box-seam me-2"></i>Refity Docker Registry
          </span>
          <div className="navbar-nav ms-auto d-flex flex-row gap-2">
            <button
              onClick={handleRefresh}
              className="btn btn-outline-light"
              disabled={isRefreshing}
            >
              {isRefreshing ? (
                <span className="spinner-border spinner-border-sm me-2"></span>
              ) : (
                <i className="bi bi-arrow-clockwise me-1"></i>
              )}
              Refresh
            </button>
            <button onClick={handleLogout} className="btn btn-outline-light">
              <i className="bi bi-box-arrow-right me-1"></i>Logout
            </button>
          </div>
        </div>
      </nav>

      <div className="container-fluid px-0">
        <div className="row mb-4 g-3">
          <div className="col-md-4">
            <StatCard
              icon="bi-images"
              label="Total Images"
              value={data?.total_images || 0}
              gradient="primary"
            />
          </div>
          <div className="col-md-4">
            <StatCard
              icon="bi-folder"
              label="Total Groups"
              value={data?.groups?.length || 0}
              gradient="success"
            />
          </div>
          <div className="col-md-4">
            <StatCard
              icon="bi-hdd"
              label="Total Size"
              value={formatBytes(data?.total_size || 0)}
              gradient="warning"
            />
          </div>
        </div>

        <div className="mb-4">
          <button
            onClick={() => setShowCreateGroupModal(true)}
            className="btn btn-primary"
          >
            <i className="bi bi-plus-circle me-2"></i>Create New Group
          </button>
        </div>

        <div className="card">
          <div className="card-header">
            <h5 className="mb-0">
              <i className="bi bi-folder me-2"></i>Groups
            </h5>
            <small className="text-muted">List of all groups and their repositories</small>
          </div>
          <div className="card-body">
            {data?.groups && data.groups.length > 0 ? (
              <div className="row g-3">
                {data.groups.map((group) => (
                  <div key={group.name} className="col-md-4">
                    <Link
                      to={`/group/${encodeURIComponent(group.name)}`}
                      className="text-decoration-none"
                    >
                      <div className="card h-100 group-card">
                        <div className="card-body">
                          <h6 className="card-title">
                            <i className="bi bi-folder-fill me-2 text-primary"></i>
                            {group.name}
                          </h6>
                          <p className="card-text text-muted mb-0">
                            <i className="bi bi-box me-1"></i>
                            {group.repositories} {group.repositories === 1 ? 'repository' : 'repositories'}
                          </p>
                        </div>
                      </div>
                    </Link>
                  </div>
                ))}
              </div>
            ) : (
              <p className="text-muted mb-0">No groups found. Create a group to get started.</p>
            )}
          </div>
        </div>
      </div>

      <CreateGroupModal
        show={showCreateGroupModal}
        onClose={() => setShowCreateGroupModal(false)}
        onCreated={handleGroupCreated}
      />
    </div>
  );
}

export default Dashboard;

